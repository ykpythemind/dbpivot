package control

const (
	CmdUse    = "use"
	CmdStatus = "status"
)

// Request is the wire format sent by the CLI to the daemon over the Unix
// socket (one JSON object per line).
type Request struct {
	Cmd       string            `json:"cmd"`
	Target    string            `json:"target,omitempty"`
	Variables map[string]string `json:"variables,omitempty"`
}

// Response is the wire format the daemon writes back.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	// `use`
	Target   string         `json:"target,omitempty"`
	Switched []SwitchResult `json:"switched,omitempty"`
	Missing  []string       `json:"missing,omitempty"`
	Probes   []ProbeResult  `json:"probes,omitempty"`

	// `status`
	Ports         map[string]int   `json:"ports,omitempty"` // adapter -> listen port
	CurrentTarget string           `json:"current_target,omitempty"`
	Databases     []DatabaseStatus `json:"databases,omitempty"`
}

type SwitchResult struct {
	VirtualName      string `json:"virtual_name"`
	Previous         string `json:"previous,omitempty"`
	PreviousDatabase string `json:"previous_database,omitempty"`
	Current          string `json:"current,omitempty"`
	CurrentDatabase  string `json:"current_database,omitempty"`
	ClosedConns      int    `json:"closed_conns"`
	// Skipped is true when this database does not declare the requested
	// target. The database is now inactive; client connections to it will
	// receive a clean PG ErrorResponse until the next switch lands on a
	// target it does declare.
	Skipped bool `json:"skipped,omitempty"`
}

// ProbeResult reports a post-switch connect + auth + `select 1` health probe
// against one database's active target.
type ProbeResult struct {
	VirtualName string `json:"virtual_name"`
	Target      string `json:"target,omitempty"`
	Database    string `json:"database,omitempty"`
	Addr        string `json:"addr,omitempty"`
	OK          bool   `json:"ok"`
	Err         string `json:"err,omitempty"`
}

type DatabaseStatus struct {
	VirtualName     string `json:"virtual_name"`
	Adapter         string `json:"adapter,omitempty"`
	Current         string `json:"current,omitempty"`
	CurrentDatabase string `json:"current_database,omitempty"`
	CurrentHost     string `json:"current_host,omitempty"`
	CurrentPort     int    `json:"current_port,omitempty"`
	ActiveConns     int    `json:"active_conns"`
	// Inactive is true when the database has no current target (e.g. it
	// was skipped by the last `use <target>` because it does not declare
	// that target name).
	Inactive bool `json:"inactive,omitempty"`
}
