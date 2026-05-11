package control

const (
	CmdSwitch = "switch"
	CmdStatus = "status"
	CmdList   = "list"
	CmdReload = "reload"
)

// Request is the wire format sent by the CLI to the daemon over the Unix
// socket (one JSON object per line).
type Request struct {
	Cmd         string            `json:"cmd"`
	VirtualName string            `json:"virtual_name,omitempty"`
	Target      string            `json:"target,omitempty"`
	Variables   map[string]string `json:"variables,omitempty"`
}

// Response is the wire format the daemon writes back.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	// `switch`
	VirtualName      string   `json:"virtual_name,omitempty"`
	Previous         string   `json:"previous,omitempty"`
	PreviousDatabase string   `json:"previous_database,omitempty"`
	Current          string   `json:"current,omitempty"`
	CurrentDatabase  string   `json:"current_database,omitempty"`
	ClosedConns      int      `json:"closed_conns,omitempty"`
	Missing          []string `json:"missing,omitempty"`

	// `status`
	Port      int              `json:"port,omitempty"`
	Databases []DatabaseStatus `json:"databases,omitempty"`

	// `list` (also reuses Databases above for status info)
	ForwardTargets map[string]ForwardTargetInfo `json:"forward_targets,omitempty"`
	ListDatabases  []DatabaseList               `json:"list_databases,omitempty"`

	// `reload`
	DatabasesUpdated int      `json:"databases_updated,omitempty"`
	DroppedConns     int      `json:"dropped_conns,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

type DatabaseStatus struct {
	VirtualName     string `json:"virtual_name"`
	Current         string `json:"current"`
	CurrentDatabase string `json:"current_database"`
	CurrentHost     string `json:"current_host"`
	CurrentPort     int    `json:"current_port"`
	ActiveConns     int    `json:"active_conns"`
}

type DatabaseList struct {
	VirtualName string       `json:"virtual_name"`
	Default     string       `json:"default"`
	Current     string       `json:"current"`
	Targets     []TargetInfo `json:"targets"`
}

type TargetInfo struct {
	Name              string   `json:"name"`
	Host              string   `json:"host,omitempty"`
	Port              int      `json:"port,omitempty"`
	ForwardTo         string   `json:"forward_to,omitempty"`
	User              string   `json:"user,omitempty"`
	DatabaseTemplate  string   `json:"database_template"`
	RequiredVariables []string `json:"required_variables"`
}

type ForwardTargetInfo struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}
