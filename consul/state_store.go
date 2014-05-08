package consul

import (
	"fmt"
	"github.com/armon/gomdb"
	"github.com/hashicorp/consul/consul/structs"
	"io"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"strings"
)

const (
	dbNodes                  = "nodes"
	dbServices               = "services"
	dbChecks                 = "checks"
	dbKVS                    = "kvs"
	dbSessions               = "sessions"
	dbSessionChecks          = "sessionChecks"
	dbMaxMapSize32bit uint64 = 512 * 1024 * 1024       // 512MB maximum size
	dbMaxMapSize64bit uint64 = 32 * 1024 * 1024 * 1024 // 32GB maximum size
)

// The StateStore is responsible for maintaining all the Consul
// state. It is manipulated by the FSM which maintains consistency
// through the use of Raft. The goals of the StateStore are to provide
// high concurrency for read operations without blocking writes, and
// to provide write availability in the face of reads. The current
// implementation uses the Lightning Memory-Mapped Database (MDB).
// This gives us Multi-Version Concurrency Control for "free"
type StateStore struct {
	logger            *log.Logger
	path              string
	env               *mdb.Env
	nodeTable         *MDBTable
	serviceTable      *MDBTable
	checkTable        *MDBTable
	kvsTable          *MDBTable
	sessionTable      *MDBTable
	sessionCheckTable *MDBTable
	tables            MDBTables
	watch             map[*MDBTable]*NotifyGroup
	queryTables       map[string]MDBTables
}

// StateSnapshot is used to provide a point-in-time snapshot
// It works by starting a readonly transaction against all tables.
type StateSnapshot struct {
	store     *StateStore
	tx        *MDBTxn
	lastIndex uint64
}

// sessionCheck is used to create a many-to-one table such
// that each check registered by a session can be mapped back
// to the session row.
type sessionCheck struct {
	Node    string
	CheckID string
	Session string
}

// Close is used to abort the transaction and allow for cleanup
func (s *StateSnapshot) Close() error {
	s.tx.Abort()
	return nil
}

// NewStateStore is used to create a new state store
func NewStateStore(logOutput io.Writer) (*StateStore, error) {
	// Create a new temp dir
	path, err := ioutil.TempDir("", "consul")
	if err != nil {
		return nil, err
	}

	// Open the env
	env, err := mdb.NewEnv()
	if err != nil {
		return nil, err
	}

	s := &StateStore{
		logger: log.New(logOutput, "", log.LstdFlags),
		path:   path,
		env:    env,
		watch:  make(map[*MDBTable]*NotifyGroup),
	}

	// Ensure we can initialize
	if err := s.initialize(); err != nil {
		env.Close()
		os.RemoveAll(path)
		return nil, err
	}
	return s, nil
}

// Close is used to safely shutdown the state store
func (s *StateStore) Close() error {
	s.env.Close()
	os.RemoveAll(s.path)
	return nil
}

// initialize is used to setup the store for use
func (s *StateStore) initialize() error {
	// Setup the Env first
	if err := s.env.SetMaxDBs(mdb.DBI(32)); err != nil {
		return err
	}

	// Set the maximum db size based on 32/64bit. Since we are
	// doing an mmap underneath, we need to limit our use of virtual
	// address space on 32bit, but don't have to care on 64bit.
	dbSize := dbMaxMapSize32bit
	if runtime.GOARCH == "amd64" {
		dbSize = dbMaxMapSize64bit
	}

	// Increase the maximum map size
	if err := s.env.SetMapSize(dbSize); err != nil {
		return err
	}

	// Optimize our flags for speed over safety, since the Raft log + snapshots
	// are durable. We treat this as an ephemeral in-memory DB, since we nuke
	// the data anyways.
	var flags uint = mdb.NOMETASYNC | mdb.NOSYNC | mdb.NOTLS
	if err := s.env.Open(s.path, flags, 0755); err != nil {
		return err
	}

	// Tables use a generic struct encoder
	encoder := func(obj interface{}) []byte {
		buf, err := structs.Encode(255, obj)
		if err != nil {
			panic(err)
		}
		return buf[1:]
	}

	// Setup our tables
	s.nodeTable = &MDBTable{
		Name: dbNodes,
		Indexes: map[string]*MDBIndex{
			"id": &MDBIndex{
				Unique: true,
				Fields: []string{"Node"},
			},
		},
		Decoder: func(buf []byte) interface{} {
			out := new(structs.Node)
			if err := structs.Decode(buf, out); err != nil {
				panic(err)
			}
			return out
		},
	}

	s.serviceTable = &MDBTable{
		Name: dbServices,
		Indexes: map[string]*MDBIndex{
			"id": &MDBIndex{
				Unique: true,
				Fields: []string{"Node", "ServiceID"},
			},
			"service": &MDBIndex{
				AllowBlank: true,
				Fields:     []string{"ServiceName"},
			},
		},
		Decoder: func(buf []byte) interface{} {
			out := new(structs.ServiceNode)
			if err := structs.Decode(buf, out); err != nil {
				panic(err)
			}
			return out
		},
	}

	s.checkTable = &MDBTable{
		Name: dbChecks,
		Indexes: map[string]*MDBIndex{
			"id": &MDBIndex{
				Unique: true,
				Fields: []string{"Node", "CheckID"},
			},
			"status": &MDBIndex{
				Fields: []string{"Status"},
			},
			"service": &MDBIndex{
				AllowBlank: true,
				Fields:     []string{"ServiceName"},
			},
			"node": &MDBIndex{
				AllowBlank: true,
				Fields:     []string{"Node", "ServiceID"},
			},
		},
		Decoder: func(buf []byte) interface{} {
			out := new(structs.HealthCheck)
			if err := structs.Decode(buf, out); err != nil {
				panic(err)
			}
			return out
		},
	}

	s.kvsTable = &MDBTable{
		Name: dbKVS,
		Indexes: map[string]*MDBIndex{
			"id": &MDBIndex{
				Unique: true,
				Fields: []string{"Key"},
			},
			"id_prefix": &MDBIndex{
				Virtual:   true,
				RealIndex: "id",
				Fields:    []string{"Key"},
				IdxFunc:   DefaultIndexPrefixFunc,
			},
		},
		Decoder: func(buf []byte) interface{} {
			out := new(structs.DirEntry)
			if err := structs.Decode(buf, out); err != nil {
				panic(err)
			}
			return out
		},
	}

	s.sessionTable = &MDBTable{
		Name: dbSessions,
		Indexes: map[string]*MDBIndex{
			"id": &MDBIndex{
				Unique: true,
				Fields: []string{"ID"},
			},
			"node": &MDBIndex{
				AllowBlank: true,
				Fields:     []string{"Node"},
			},
		},
		Decoder: func(buf []byte) interface{} {
			out := new(structs.Session)
			if err := structs.Decode(buf, out); err != nil {
				panic(err)
			}
			return out
		},
	}

	s.sessionCheckTable = &MDBTable{
		Name: dbSessionChecks,
		Indexes: map[string]*MDBIndex{
			"id": &MDBIndex{
				Unique: true,
				Fields: []string{"Node", "CheckID", "Session"},
			},
		},
		Decoder: func(buf []byte) interface{} {
			out := new(sessionCheck)
			if err := structs.Decode(buf, out); err != nil {
				panic(err)
			}
			return out
		},
	}

	// Store the set of tables
	s.tables = []*MDBTable{s.nodeTable, s.serviceTable, s.checkTable,
		s.kvsTable, s.sessionTable, s.sessionCheckTable}
	for _, table := range s.tables {
		table.Env = s.env
		table.Encoder = encoder
		if err := table.Init(); err != nil {
			return err
		}

		// Setup a notification group per table
		s.watch[table] = &NotifyGroup{}
	}

	// Setup the query tables
	s.queryTables = map[string]MDBTables{
		"Nodes":             MDBTables{s.nodeTable},
		"Services":          MDBTables{s.serviceTable},
		"ServiceNodes":      MDBTables{s.nodeTable, s.serviceTable},
		"NodeServices":      MDBTables{s.nodeTable, s.serviceTable},
		"ChecksInState":     MDBTables{s.checkTable},
		"NodeChecks":        MDBTables{s.checkTable},
		"ServiceChecks":     MDBTables{s.checkTable},
		"CheckServiceNodes": MDBTables{s.nodeTable, s.serviceTable, s.checkTable},
		"NodeInfo":          MDBTables{s.nodeTable, s.serviceTable, s.checkTable},
		"NodeDump":          MDBTables{s.nodeTable, s.serviceTable, s.checkTable},
		"KVSGet":            MDBTables{s.kvsTable},
		"KVSList":           MDBTables{s.kvsTable},
		"KVSListKeys":       MDBTables{s.kvsTable},
	}
	return nil
}

// Watch is used to subscribe a channel to a set of MDBTables
func (s *StateStore) Watch(tables MDBTables, notify chan struct{}) {
	for _, t := range tables {
		s.watch[t].Wait(notify)
	}
}

// QueryTables returns the Tables that are queried for a given query
func (s *StateStore) QueryTables(q string) MDBTables {
	return s.queryTables[q]
}

// EnsureNode is used to ensure a given node exists, with the provided address
func (s *StateStore) EnsureNode(index uint64, node structs.Node) error {
	// Start a new txn
	tx, err := s.nodeTable.StartTxn(false, nil)
	if err != nil {
		return err
	}
	defer tx.Abort()

	if err := s.nodeTable.InsertTxn(tx, node); err != nil {
		return err
	}
	if err := s.nodeTable.SetLastIndexTxn(tx, index); err != nil {
		return err
	}
	defer s.watch[s.nodeTable].Notify()
	return tx.Commit()
}

// GetNode returns all the address of the known and if it was found
func (s *StateStore) GetNode(name string) (uint64, bool, string) {
	idx, res, err := s.nodeTable.Get("id", name)
	if err != nil {
		s.logger.Printf("[ERR] consul.state: Error during node lookup: %v", err)
		return 0, false, ""
	}
	if len(res) == 0 {
		return idx, false, ""
	}
	return idx, true, res[0].(*structs.Node).Address
}

// GetNodes returns all the known nodes, the slice alternates between
// the node name and address
func (s *StateStore) Nodes() (uint64, structs.Nodes) {
	idx, res, err := s.nodeTable.Get("id")
	if err != nil {
		s.logger.Printf("[ERR] consul.state: Error getting nodes: %v", err)
	}
	results := make([]structs.Node, len(res))
	for i, r := range res {
		results[i] = *r.(*structs.Node)
	}
	return idx, results
}

// EnsureService is used to ensure a given node exposes a service
func (s *StateStore) EnsureService(index uint64, node string, ns *structs.NodeService) error {
	tables := MDBTables{s.nodeTable, s.serviceTable}
	tx, err := tables.StartTxn(false)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	// Ensure the node exists
	res, err := s.nodeTable.GetTxn(tx, "id", node)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		return fmt.Errorf("Missing node registration")
	}

	// Create the entry
	entry := structs.ServiceNode{
		Node:        node,
		ServiceID:   ns.ID,
		ServiceName: ns.Service,
		ServiceTags: ns.Tags,
		ServicePort: ns.Port,
	}

	// Ensure the service entry is set
	if err := s.serviceTable.InsertTxn(tx, &entry); err != nil {
		return err
	}
	if err := s.serviceTable.SetLastIndexTxn(tx, index); err != nil {
		return err
	}
	defer s.watch[s.serviceTable].Notify()
	return tx.Commit()
}

// NodeServices is used to return all the services of a given node
func (s *StateStore) NodeServices(name string) (uint64, *structs.NodeServices) {
	tables := s.queryTables["NodeServices"]
	tx, err := tables.StartTxn(true)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()
	return s.parseNodeServices(tables, tx, name)
}

// parseNodeServices is used to get the services belonging to a
// node, using a given txn
func (s *StateStore) parseNodeServices(tables MDBTables, tx *MDBTxn, name string) (uint64, *structs.NodeServices) {
	ns := &structs.NodeServices{
		Services: make(map[string]*structs.NodeService),
	}

	// Get the maximum index
	index, err := tables.LastIndexTxn(tx)
	if err != nil {
		panic(fmt.Errorf("Failed to get last index: %v", err))
	}

	// Get the node first
	res, err := s.nodeTable.GetTxn(tx, "id", name)
	if err != nil {
		s.logger.Printf("[ERR] consul.state: Failed to get node: %v", err)
	}
	if len(res) == 0 {
		return index, nil
	}

	// Set the address
	node := res[0].(*structs.Node)
	ns.Node = *node

	// Get the services
	res, err = s.serviceTable.GetTxn(tx, "id", name)
	if err != nil {
		s.logger.Printf("[ERR] consul.state: Failed to get node '%s' services: %v", name, err)
	}

	// Add each service
	for _, r := range res {
		service := r.(*structs.ServiceNode)
		srv := &structs.NodeService{
			ID:      service.ServiceID,
			Service: service.ServiceName,
			Tags:    service.ServiceTags,
			Port:    service.ServicePort,
		}
		ns.Services[srv.ID] = srv
	}
	return index, ns
}

// DeleteNodeService is used to delete a node service
func (s *StateStore) DeleteNodeService(index uint64, node, id string) error {
	tables := MDBTables{s.serviceTable, s.checkTable}
	tx, err := tables.StartTxn(false)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	if n, err := s.serviceTable.DeleteTxn(tx, "id", node, id); err != nil {
		return err
	} else if n > 0 {
		if err := s.serviceTable.SetLastIndexTxn(tx, index); err != nil {
			return err
		}
		defer s.watch[s.serviceTable].Notify()
	}
	if n, err := s.checkTable.DeleteTxn(tx, "node", node, id); err != nil {
		return err
	} else if n > 0 {
		if err := s.checkTable.SetLastIndexTxn(tx, index); err != nil {
			return err
		}
		defer s.watch[s.checkTable].Notify()
	}
	return tx.Commit()
}

// DeleteNode is used to delete a node and all it's services
func (s *StateStore) DeleteNode(index uint64, node string) error {
	tables := MDBTables{s.nodeTable, s.serviceTable, s.checkTable}
	tx, err := tables.StartTxn(false)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	if n, err := s.serviceTable.DeleteTxn(tx, "id", node); err != nil {
		return err
	} else if n > 0 {
		if err := s.serviceTable.SetLastIndexTxn(tx, index); err != nil {
			return err
		}
		defer s.watch[s.serviceTable].Notify()
	}
	if n, err := s.checkTable.DeleteTxn(tx, "id", node); err != nil {
		return err
	} else if n > 0 {
		if err := s.checkTable.SetLastIndexTxn(tx, index); err != nil {
			return err
		}
		defer s.watch[s.checkTable].Notify()
	}
	if n, err := s.nodeTable.DeleteTxn(tx, "id", node); err != nil {
		return err
	} else if n > 0 {
		if err := s.nodeTable.SetLastIndexTxn(tx, index); err != nil {
			return err
		}
		defer s.watch[s.nodeTable].Notify()
	}
	return tx.Commit()
}

// Services is used to return all the services with a list of associated tags
func (s *StateStore) Services() (uint64, map[string][]string) {
	services := make(map[string][]string)
	idx, res, err := s.serviceTable.Get("id")
	if err != nil {
		s.logger.Printf("[ERR] consul.state: Failed to get services: %v", err)
		return idx, services
	}
	for _, r := range res {
		srv := r.(*structs.ServiceNode)
		tags, ok := services[srv.ServiceName]
		if !ok {
			services[srv.ServiceName] = make([]string, 0)
		}

		for _, tag := range srv.ServiceTags {
			if !strContains(tags, tag) {
				tags = append(tags, tag)
				services[srv.ServiceName] = tags
			}
		}
	}
	return idx, services
}

// ServiceNodes returns the nodes associated with a given service
func (s *StateStore) ServiceNodes(service string) (uint64, structs.ServiceNodes) {
	tables := s.queryTables["ServiceNodes"]
	tx, err := tables.StartTxn(true)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	idx, err := tables.LastIndexTxn(tx)
	if err != nil {
		panic(fmt.Errorf("Failed to get last index: %v", err))
	}

	res, err := s.serviceTable.GetTxn(tx, "service", service)
	return idx, s.parseServiceNodes(tx, s.nodeTable, res, err)
}

// ServiceTagNodes returns the nodes associated with a given service matching a tag
func (s *StateStore) ServiceTagNodes(service, tag string) (uint64, structs.ServiceNodes) {
	tables := s.queryTables["ServiceNodes"]
	tx, err := tables.StartTxn(true)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	idx, err := tables.LastIndexTxn(tx)
	if err != nil {
		panic(fmt.Errorf("Failed to get last index: %v", err))
	}

	res, err := s.serviceTable.GetTxn(tx, "service", service)
	res = serviceTagFilter(res, tag)
	return idx, s.parseServiceNodes(tx, s.nodeTable, res, err)
}

// serviceTagFilter is used to filter a list of *structs.ServiceNode which do
// not have the specified tag
func serviceTagFilter(l []interface{}, tag string) []interface{} {
	n := len(l)
	for i := 0; i < n; i++ {
		srv := l[i].(*structs.ServiceNode)
		if !strContains(srv.ServiceTags, tag) {
			l[i], l[n-1] = l[n-1], nil
			i--
			n--
		}
	}
	return l[:n]
}

// parseServiceNodes parses results ServiceNodes and ServiceTagNodes
func (s *StateStore) parseServiceNodes(tx *MDBTxn, table *MDBTable, res []interface{}, err error) structs.ServiceNodes {
	nodes := make(structs.ServiceNodes, len(res))
	if err != nil {
		s.logger.Printf("[ERR] consul.state: Failed to get service nodes: %v", err)
		return nodes
	}

	for i, r := range res {
		srv := r.(*structs.ServiceNode)

		// Get the address of the node
		nodeRes, err := table.GetTxn(tx, "id", srv.Node)
		if err != nil || len(nodeRes) != 1 {
			s.logger.Printf("[ERR] consul.state: Failed to join service node %#v with node: %v", *srv, err)
			continue
		}
		srv.Address = nodeRes[0].(*structs.Node).Address

		nodes[i] = *srv
	}

	return nodes
}

// EnsureCheck is used to create a check or updates it's state
func (s *StateStore) EnsureCheck(index uint64, check *structs.HealthCheck) error {
	// Ensure we have a status
	if check.Status == "" {
		check.Status = structs.HealthUnknown
	}

	// Start the txn
	tables := MDBTables{s.nodeTable, s.serviceTable, s.checkTable}
	tx, err := tables.StartTxn(false)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	// Ensure the node exists
	res, err := s.nodeTable.GetTxn(tx, "id", check.Node)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		return fmt.Errorf("Missing node registration")
	}

	// Ensure the service exists if specified
	if check.ServiceID != "" {
		res, err = s.serviceTable.GetTxn(tx, "id", check.Node, check.ServiceID)
		if err != nil {
			return err
		}
		if len(res) == 0 {
			return fmt.Errorf("Missing service registration")
		}
		// Ensure we set the correct service
		srv := res[0].(*structs.ServiceNode)
		check.ServiceName = srv.ServiceName
	}

	// Ensure the check is set
	if err := s.checkTable.InsertTxn(tx, check); err != nil {
		return err
	}
	if err := s.checkTable.SetLastIndexTxn(tx, index); err != nil {
		return err
	}
	defer s.watch[s.checkTable].Notify()
	return tx.Commit()
}

// DeleteNodeCheck is used to delete a node health check
func (s *StateStore) DeleteNodeCheck(index uint64, node, id string) error {
	tx, err := s.checkTable.StartTxn(false, nil)
	if err != nil {
		return err
	}
	defer tx.Abort()

	if n, err := s.checkTable.DeleteTxn(tx, "id", node, id); err != nil {
		return err
	} else if n > 0 {
		if err := s.checkTable.SetLastIndexTxn(tx, index); err != nil {
			return err
		}
		defer s.watch[s.checkTable].Notify()
	}
	return tx.Commit()
}

// NodeChecks is used to get all the checks for a node
func (s *StateStore) NodeChecks(node string) (uint64, structs.HealthChecks) {
	return s.parseHealthChecks(s.checkTable.Get("id", node))
}

// ServiceChecks is used to get all the checks for a service
func (s *StateStore) ServiceChecks(service string) (uint64, structs.HealthChecks) {
	return s.parseHealthChecks(s.checkTable.Get("service", service))
}

// CheckInState is used to get all the checks for a service in a given state
func (s *StateStore) ChecksInState(state string) (uint64, structs.HealthChecks) {
	return s.parseHealthChecks(s.checkTable.Get("status", state))
}

// parseHealthChecks is used to handle the resutls of a Get against
// the checkTable
func (s *StateStore) parseHealthChecks(idx uint64, res []interface{}, err error) (uint64, structs.HealthChecks) {
	results := make([]*structs.HealthCheck, len(res))
	if err != nil {
		s.logger.Printf("[ERR] consul.state: Failed to get health checks: %v", err)
		return idx, results
	}
	for i, r := range res {
		results[i] = r.(*structs.HealthCheck)
	}
	return idx, results
}

// CheckServiceNodes returns the nodes associated with a given service, along
// with any associated check
func (s *StateStore) CheckServiceNodes(service string) (uint64, structs.CheckServiceNodes) {
	tables := s.queryTables["CheckServiceNodes"]
	tx, err := tables.StartTxn(true)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	idx, err := tables.LastIndexTxn(tx)
	if err != nil {
		panic(fmt.Errorf("Failed to get last index: %v", err))
	}

	res, err := s.serviceTable.GetTxn(tx, "service", service)
	return idx, s.parseCheckServiceNodes(tx, res, err)
}

// CheckServiceNodes returns the nodes associated with a given service, along
// with any associated checks
func (s *StateStore) CheckServiceTagNodes(service, tag string) (uint64, structs.CheckServiceNodes) {
	tables := s.queryTables["CheckServiceNodes"]
	tx, err := tables.StartTxn(true)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	idx, err := tables.LastIndexTxn(tx)
	if err != nil {
		panic(fmt.Errorf("Failed to get last index: %v", err))
	}

	res, err := s.serviceTable.GetTxn(tx, "service", service)
	res = serviceTagFilter(res, tag)
	return idx, s.parseCheckServiceNodes(tx, res, err)
}

// parseCheckServiceNodes parses results CheckServiceNodes and CheckServiceTagNodes
func (s *StateStore) parseCheckServiceNodes(tx *MDBTxn, res []interface{}, err error) structs.CheckServiceNodes {
	nodes := make(structs.CheckServiceNodes, len(res))
	if err != nil {
		s.logger.Printf("[ERR] consul.state: Failed to get service nodes: %v", err)
		return nodes
	}

	for i, r := range res {
		srv := r.(*structs.ServiceNode)

		// Get the node
		nodeRes, err := s.nodeTable.GetTxn(tx, "id", srv.Node)
		if err != nil || len(nodeRes) != 1 {
			s.logger.Printf("[ERR] consul.state: Failed to join service node %#v with node: %v", *srv, err)
			continue
		}

		// Get any associated checks of the service
		res, err := s.checkTable.GetTxn(tx, "node", srv.Node, srv.ServiceID)
		_, checks := s.parseHealthChecks(0, res, err)

		// Get any checks of the node, not assciated with any service
		res, err = s.checkTable.GetTxn(tx, "node", srv.Node, "")
		_, nodeChecks := s.parseHealthChecks(0, res, err)
		checks = append(checks, nodeChecks...)

		// Setup the node
		nodes[i].Node = *nodeRes[0].(*structs.Node)
		nodes[i].Service = structs.NodeService{
			ID:      srv.ServiceID,
			Service: srv.ServiceName,
			Tags:    srv.ServiceTags,
			Port:    srv.ServicePort,
		}
		nodes[i].Checks = checks
	}

	return nodes
}

// NodeInfo is used to generate the full info about a node.
func (s *StateStore) NodeInfo(node string) (uint64, structs.NodeDump) {
	tables := s.queryTables["NodeInfo"]
	tx, err := tables.StartTxn(true)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	idx, err := tables.LastIndexTxn(tx)
	if err != nil {
		panic(fmt.Errorf("Failed to get last index: %v", err))
	}

	res, err := s.nodeTable.GetTxn(tx, "id", node)
	return idx, s.parseNodeInfo(tx, res, err)
}

// NodeDump is used to generate the NodeInfo for all nodes. This is very expensive,
// and should generally be avoided for programatic access.
func (s *StateStore) NodeDump() (uint64, structs.NodeDump) {
	tables := s.queryTables["NodeDump"]
	tx, err := tables.StartTxn(true)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	idx, err := tables.LastIndexTxn(tx)
	if err != nil {
		panic(fmt.Errorf("Failed to get last index: %v", err))
	}

	res, err := s.nodeTable.GetTxn(tx, "id")
	return idx, s.parseNodeInfo(tx, res, err)
}

// parseNodeInfo is used to scan over the results of a node
// iteration and generate a NodeDump
func (s *StateStore) parseNodeInfo(tx *MDBTxn, res []interface{}, err error) structs.NodeDump {
	dump := make(structs.NodeDump, 0, len(res))
	if err != nil {
		s.logger.Printf("[ERR] consul.state: Failed to get nodes: %v", err)
		return dump
	}

	for _, r := range res {
		// Copy the address and node
		node := r.(*structs.Node)
		info := &structs.NodeInfo{
			Node:    node.Node,
			Address: node.Address,
		}

		// Get any services of the node
		res, err = s.serviceTable.GetTxn(tx, "id", node.Node)
		if err != nil {
			s.logger.Printf("[ERR] consul.state: Failed to get node services: %v", err)
		}
		info.Services = make([]*structs.NodeService, 0, len(res))
		for _, r := range res {
			service := r.(*structs.ServiceNode)
			srv := &structs.NodeService{
				ID:      service.ServiceID,
				Service: service.ServiceName,
				Tags:    service.ServiceTags,
				Port:    service.ServicePort,
			}
			info.Services = append(info.Services, srv)
		}

		// Get any checks of the node
		res, err = s.checkTable.GetTxn(tx, "node", node.Node)
		if err != nil {
			s.logger.Printf("[ERR] consul.state: Failed to get node checks: %v", err)
		}
		info.Checks = make([]*structs.HealthCheck, 0, len(res))
		for _, r := range res {
			chk := r.(*structs.HealthCheck)
			info.Checks = append(info.Checks, chk)
		}

		// Add the node info
		dump = append(dump, info)
	}
	return dump
}

// KVSSet is used to create or update a KV entry
func (s *StateStore) KVSSet(index uint64, d *structs.DirEntry) error {
	// Start a new txn
	tx, err := s.kvsTable.StartTxn(false, nil)
	if err != nil {
		return err
	}
	defer tx.Abort()

	// Get the existing node
	res, err := s.kvsTable.GetTxn(tx, "id", d.Key)
	if err != nil {
		return err
	}

	// Set the create and modify times
	if len(res) == 0 {
		d.CreateIndex = index
	} else {
		d.CreateIndex = res[0].(*structs.DirEntry).CreateIndex
	}
	d.ModifyIndex = index

	if err := s.kvsTable.InsertTxn(tx, d); err != nil {
		return err
	}
	if err := s.kvsTable.SetLastIndexTxn(tx, index); err != nil {
		return err
	}
	defer s.watch[s.kvsTable].Notify()
	return tx.Commit()
}

// KVSRestore is used to restore a DirEntry. It should only be used when
// doing a restore, otherwise KVSSet should be used.
func (s *StateStore) KVSRestore(d *structs.DirEntry) error {
	// Start a new txn
	tx, err := s.kvsTable.StartTxn(false, nil)
	if err != nil {
		return err
	}
	defer tx.Abort()

	if err := s.kvsTable.InsertTxn(tx, d); err != nil {
		return err
	}
	return tx.Commit()
}

// KVSGet is used to get a KV entry
func (s *StateStore) KVSGet(key string) (uint64, *structs.DirEntry, error) {
	idx, res, err := s.kvsTable.Get("id", key)
	var d *structs.DirEntry
	if len(res) > 0 {
		d = res[0].(*structs.DirEntry)
	}
	return idx, d, err
}

// KVSList is used to list all KV entries with a prefix
func (s *StateStore) KVSList(prefix string) (uint64, structs.DirEntries, error) {
	idx, res, err := s.kvsTable.Get("id_prefix", prefix)
	ents := make(structs.DirEntries, len(res))
	for idx, r := range res {
		ents[idx] = r.(*structs.DirEntry)
	}
	return idx, ents, err
}

// KVSListKeys is used to list keys with a prefix, and up to a given seperator
func (s *StateStore) KVSListKeys(prefix, seperator string) (uint64, []string, error) {
	tx, err := s.kvsTable.StartTxn(true, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Abort()

	idx, err := s.kvsTable.LastIndexTxn(tx)
	if err != nil {
		return 0, nil, err
	}

	// Aggregate the stream
	stream := make(chan interface{}, 128)
	done := make(chan struct{})
	var keys []string
	go func() {
		prefixLen := len(prefix)
		sepLen := len(seperator)
		last := ""
		for raw := range stream {
			ent := raw.(*structs.DirEntry)
			after := ent.Key[prefixLen:]

			// If there is no seperator, always accumulate
			if sepLen == 0 {
				keys = append(keys, ent.Key)
				continue
			}

			// Check for the seperator
			if idx := strings.Index(after, seperator); idx >= 0 {
				toSep := ent.Key[:prefixLen+idx+sepLen]
				if last != toSep {
					keys = append(keys, toSep)
					last = toSep
				}
			} else {
				keys = append(keys, ent.Key)
			}
		}
		close(done)
	}()

	// Start the stream, and wait for completion
	err = s.kvsTable.StreamTxn(stream, tx, "id_prefix", prefix)
	<-done
	return idx, keys, err
}

// KVSDelete is used to delete a KVS entry
func (s *StateStore) KVSDelete(index uint64, key string) error {
	return s.kvsDeleteWithIndex(index, "id", key)
}

// KVSDeleteTree is used to delete all keys with a given prefix
func (s *StateStore) KVSDeleteTree(index uint64, prefix string) error {
	if prefix == "" {
		return s.kvsDeleteWithIndex(index, "id")
	}
	return s.kvsDeleteWithIndex(index, "id_prefix", prefix)
}

// kvsDeleteWithIndex does a delete with either the id or id_prefix
func (s *StateStore) kvsDeleteWithIndex(index uint64, tableIndex string, parts ...string) error {
	// Start a new txn
	tx, err := s.kvsTable.StartTxn(false, nil)
	if err != nil {
		return err
	}
	defer tx.Abort()

	num, err := s.kvsTable.DeleteTxn(tx, tableIndex, parts...)
	if err != nil {
		return err
	}

	if num > 0 {
		if err := s.kvsTable.SetLastIndexTxn(tx, index); err != nil {
			return err
		}
		defer s.watch[s.kvsTable].Notify()
	}
	return tx.Commit()
}

// KVSCheckAndSet is used to perform an atomic check-and-set
func (s *StateStore) KVSCheckAndSet(index uint64, d *structs.DirEntry) (bool, error) {
	// Start a new txn
	tx, err := s.kvsTable.StartTxn(false, nil)
	if err != nil {
		return false, err
	}
	defer tx.Abort()

	// Get the existing node
	res, err := s.kvsTable.GetTxn(tx, "id", d.Key)
	if err != nil {
		return false, err
	}

	// Get the existing node if any
	var exist *structs.DirEntry
	if len(res) > 0 {
		exist = res[0].(*structs.DirEntry)
	}

	// Use the ModifyIndex as the constraint. A modify of time of 0
	// means we are doing a set-if-not-exists, while any other value
	// means we expect that modify time.
	if d.ModifyIndex == 0 && exist != nil {
		return false, nil
	} else if d.ModifyIndex > 0 && (exist == nil || exist.ModifyIndex != d.ModifyIndex) {
		return false, nil
	}

	// Set the create and modify times
	if exist == nil {
		d.CreateIndex = index
	} else {
		d.CreateIndex = exist.CreateIndex
	}
	d.ModifyIndex = index

	if err := s.kvsTable.InsertTxn(tx, d); err != nil {
		return false, err
	}
	if err := s.kvsTable.SetLastIndexTxn(tx, index); err != nil {
		return false, err
	}
	defer s.watch[s.kvsTable].Notify()
	return true, tx.Commit()
}

// SessionCreate is used to create a new session. The
// ID will be populated on a successful return
func (s *StateStore) SessionCreate(index uint64, session *structs.Session) error {
	// Assign the create index
	session.CreateIndex = index

	// Start the transaction
	tables := MDBTables{s.nodeTable, s.checkTable,
		s.sessionTable, s.sessionCheckTable}
	tx, err := tables.StartTxn(false)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	// Verify that the node exists
	res, err := s.nodeTable.GetTxn(tx, "id", session.Node)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		return fmt.Errorf("Missing node registration")
	}

	// Verify that the checks exist and are not critical
	for _, checkId := range session.Checks {
		res, err := s.checkTable.GetTxn(tx, "id", session.Node, checkId)
		if err != nil {
			return err
		}
		if len(res) == 0 {
			return fmt.Errorf("Missing check '%s' registration", checkId)
		}
		chk := res[0].(*structs.HealthCheck)
		if chk.Status == structs.HealthCritical {
			return fmt.Errorf("Check '%s' is in %s state", checkId, chk.Status)
		}
	}

	// Generate a new session ID, verify uniqueness
	session.ID = generateUUID()
	for {
		res, err = s.sessionTable.GetTxn(tx, "id", session.ID)
		if err != nil {
			return err
		}
		// Quit if this ID is unique
		if len(res) == 0 {
			break
		}
	}

	// Insert the session
	if err := s.sessionTable.InsertTxn(tx, session); err != nil {
		return err
	}

	// Insert the check mappings
	sCheck := sessionCheck{Node: session.Node, Session: session.ID}
	for _, checkID := range session.Checks {
		sCheck.CheckID = checkID
		if err := s.sessionCheckTable.InsertTxn(tx, &sCheck); err != nil {
			return err
		}
	}

	// Trigger the update notifications
	if err := s.sessionTable.SetLastIndexTxn(tx, index); err != nil {
		return err
	}
	defer s.watch[s.sessionTable].Notify()

	if err := s.sessionCheckTable.SetLastIndexTxn(tx, index); err != nil {
		return err
	}
	defer s.watch[s.sessionCheckTable].Notify()

	return tx.Commit()
}

// SessionRestore is used to restore a session. It should only be used when
// doing a restore, otherwise SessionCreate should be used.
func (s *StateStore) SessionRestore(session *structs.Session) error {
	// Start the transaction
	tables := MDBTables{s.nodeTable, s.checkTable,
		s.sessionTable, s.sessionCheckTable}
	tx, err := tables.StartTxn(false)
	if err != nil {
		panic(fmt.Errorf("Failed to start txn: %v", err))
	}
	defer tx.Abort()

	// Insert the session
	if err := s.sessionTable.InsertTxn(tx, session); err != nil {
		return err
	}

	// Insert the check mappings
	sCheck := sessionCheck{Node: session.Node, Session: session.ID}
	for _, checkID := range session.Checks {
		sCheck.CheckID = checkID
		if err := s.sessionCheckTable.InsertTxn(tx, &sCheck); err != nil {
			return err
		}
	}

	// Trigger the update notifications
	index := session.CreateIndex
	if err := s.sessionTable.SetMaxLastIndexTxn(tx, index); err != nil {
		return err
	}
	defer s.watch[s.sessionTable].Notify()

	if err := s.sessionCheckTable.SetMaxLastIndexTxn(tx, index); err != nil {
		return err
	}
	defer s.watch[s.sessionCheckTable].Notify()

	return tx.Commit()
}

// Snapshot is used to create a point in time snapshot
func (s *StateStore) Snapshot() (*StateSnapshot, error) {
	// Begin a new txn on all tables
	tx, err := s.tables.StartTxn(true)
	if err != nil {
		return nil, err
	}

	// Determine the max index
	index, err := s.tables.LastIndexTxn(tx)
	if err != nil {
		tx.Abort()
		return nil, err
	}

	// Return the snapshot
	snap := &StateSnapshot{
		store:     s,
		tx:        tx,
		lastIndex: index,
	}
	return snap, nil
}

// LastIndex returns the last index that affects the snapshotted data
func (s *StateSnapshot) LastIndex() uint64 {
	return s.lastIndex
}

// Nodes returns all the known nodes, the slice alternates between
// the node name and address
func (s *StateSnapshot) Nodes() structs.Nodes {
	res, err := s.store.nodeTable.GetTxn(s.tx, "id")
	if err != nil {
		s.store.logger.Printf("[ERR] consul.state: Failed to get nodes: %v", err)
		return nil
	}
	results := make([]structs.Node, len(res))
	for i, r := range res {
		results[i] = *r.(*structs.Node)
	}
	return results
}

// NodeServices is used to return all the services of a given node
func (s *StateSnapshot) NodeServices(name string) *structs.NodeServices {
	_, res := s.store.parseNodeServices(s.store.tables, s.tx, name)
	return res
}

// NodeChecks is used to return all the checks of a given node
func (s *StateSnapshot) NodeChecks(node string) structs.HealthChecks {
	res, err := s.store.checkTable.GetTxn(s.tx, "id", node)
	_, checks := s.store.parseHealthChecks(s.lastIndex, res, err)
	return checks
}

// KVSDump is used to list all KV entries. It takes a channel and streams
// back *struct.DirEntry objects. This will block and should be invoked
// in a goroutine.
func (s *StateSnapshot) KVSDump(stream chan<- interface{}) error {
	return s.store.kvsTable.StreamTxn(stream, s.tx, "id")
}
