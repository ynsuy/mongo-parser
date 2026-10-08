package shell

import "fmt"

// ServerStatusInfo is what one serverStatus() printout says about the server that answered it.
//
// A bundle's shell outputs are a single moment — the moment somebody ran the command — so nothing
// here is a history. It is the one place a bundle states the connection ceiling, the resident set
// and the replica set role outright, and on a bundle with no FTDC it is the only place at all.
type ServerStatusInfo struct {
	Host         string
	Version      string
	Process      string
	Service      []string
	PID          int
	Uptime       int
	UptimeMillis int64
	CurrentTime  string
	Connections  ConnectionStats
	Memory       MemoryStats
	Operations   OperationStats
	Replication  ReplicationStats
	Locks        map[string]interface{}
	WiredTiger   map[string]interface{}
	Network      NetworkStats
	GlobalLock   map[string]interface{}
	Asserts      map[string]interface{}
}

type ConnectionStats struct {
	Current            int
	Available          int
	TotalCreated       int
	Rejected           int
	Active             int
	QueuedForEstablish int
	Threaded           int
}

// MemoryStats are the server's own figures, in megabytes, which is how serverStatus reports them.
type MemoryStats struct {
	Resident          int64
	Virtual           int64
	Mapped            int64
	MappedWithJournal int64
	Bits              int
}

type OperationStats struct {
	Iops           int
	Queued         int
	NetworkInQueue int
	ExecutionQueue int
}

type ReplicationStats struct {
	OplogMaxSizeMB int64
	OplogSizeMB    int64
	IsMaster       bool
	IsSecondary    bool
	IsArbiter      bool
	MyState        int
	Members        int
}

type NetworkStats struct {
	BytesIn     int64
	BytesOut    int64
	NumRequests int
}

// ParseServerStatus reads a serverStatus() printout as a shell wrote it.
func ParseServerStatus(content string) (*ServerStatusInfo, error) {
	data, err := ParseEJSON(content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse serverStatus: %w", err)
	}

	info := &ServerStatusInfo{
		Host:    GetString(data, "host"),
		Version: GetString(data, "version"),
		Process: GetString(data, "process"),
		PID:     GetInt(data, "pid"),
		Uptime:  GetInt(data, "uptime"),
	}

	// A mongod on 8.0 names the services it is running, which is what tells a router from a shard
	// when the process field says mongod for both.
	if services := GetArray(data, "service"); services != nil {
		for _, svc := range services {
			if s, ok := svc.(string); ok {
				info.Service = append(info.Service, s)
			}
		}
	}

	if connMap := GetMap(data, "connections"); connMap != nil {
		info.Connections = ConnectionStats{
			Current:      GetInt(connMap, "current"),
			Available:    GetInt(connMap, "available"),
			TotalCreated: GetInt(connMap, "totalCreated"),
			Rejected:     GetInt(connMap, "rejected"),
			Active:       GetInt(connMap, "active"),
			Threaded:     GetInt(connMap, "threaded"),
		}
	}

	// mem is absent on some builds, and mapped only ever appears on MMAPv1.
	if memMap := GetMap(data, "mem"); memMap != nil {
		info.Memory = MemoryStats{
			Resident: int64(GetInt(memMap, "resident")),
			Virtual:  int64(GetInt(memMap, "virtual")),
			Mapped:   int64(GetInt(memMap, "mapped")),
			Bits:     GetInt(memMap, "bits"),
		}
	}

	if replMap := GetMap(data, "repl"); replMap != nil {
		info.Replication = ReplicationStats{
			OplogMaxSizeMB: int64(GetInt(replMap, "oplogSizeMB")),
			IsMaster:       getBool(replMap, "ismaster"),
			IsSecondary:    getBool(replMap, "secondary"),
			IsArbiter:      getBool(replMap, "arbiterOnly"),
			MyState:        GetInt(replMap, "myState"),
			Members:        GetInt(replMap, "numMembers"),
		}
	}

	// The rest is kept raw: these subtrees are hundreds of counters deep and whoever asks for one
	// knows which, so naming a field here would be guessing on their behalf.
	info.Locks = GetMap(data, "locks")
	info.WiredTiger = GetMap(data, "wiredTiger")
	info.GlobalLock = GetMap(data, "globalLock")
	info.Asserts = GetMap(data, "asserts")
	info.Network = NetworkStats{
		BytesIn:     int64(GetInt(data, "network", "bytesIn")),
		BytesOut:    int64(GetInt(data, "network", "bytesOut")),
		NumRequests: GetInt(data, "network", "numRequests"),
	}

	return info, nil
}

func getBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}
