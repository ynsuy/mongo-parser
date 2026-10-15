package shell

import "fmt"

// ServerStatusInfo is what one serverStatus() printout says about the server that answered it.
//
// A bundle's shell outputs are a single moment — the moment somebody ran the command — so nothing
// here is a history. It is the one place a bundle states the connection ceiling, the resident set
// and the replica set role outright, and on a bundle with no FTDC it is the only place at all.
type ServerStatusInfo struct {
	Host        string
	Version     string
	Process     string
	Service     []string
	PID         int
	Uptime      int
	Connections ConnectionStats
	Memory      MemoryStats
	Replication ReplicationStats
	Network     NetworkStats
}

type ConnectionStats struct {
	Current      int
	Available    int
	TotalCreated int
	Rejected     int
}

// MemoryStats are the server's own figures, in megabytes, which is how serverStatus reports them.
type MemoryStats struct {
	Resident int64
	Virtual  int64
}

type ReplicationStats struct {
	IsMaster    bool
	IsSecondary bool
	IsArbiter   bool
	MyState     int
	Members     int
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
		}
	}

	// mem is absent on some builds.
	if memMap := GetMap(data, "mem"); memMap != nil {
		info.Memory = MemoryStats{
			Resident: int64(GetInt(memMap, "resident")),
			Virtual:  int64(GetInt(memMap, "virtual")),
		}
	}

	if replMap := GetMap(data, "repl"); replMap != nil {
		info.Replication = ReplicationStats{
			IsMaster:    getBool(replMap, "ismaster"),
			IsSecondary: getBool(replMap, "secondary"),
			IsArbiter:   getBool(replMap, "arbiterOnly"),
			MyState:     GetInt(replMap, "myState"),
			Members:     GetInt(replMap, "numMembers"),
		}
	}

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
