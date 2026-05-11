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

	// `status`
	Port          int              `json:"port,omitempty"`
	CurrentTarget string           `json:"current_target,omitempty"`
	Databases     []DatabaseStatus `json:"databases,omitempty"`
}

type SwitchResult struct {
	VirtualName      string `json:"virtual_name"`
	Previous         string `json:"previous,omitempty"`
	PreviousDatabase string `json:"previous_database,omitempty"`
	Current          string `json:"current"`
	CurrentDatabase  string `json:"current_database"`
	ClosedConns      int    `json:"closed_conns"`
}

type DatabaseStatus struct {
	VirtualName     string `json:"virtual_name"`
	Current         string `json:"current"`
	CurrentDatabase string `json:"current_database"`
	CurrentHost     string `json:"current_host"`
	CurrentPort     int    `json:"current_port"`
	ActiveConns     int    `json:"active_conns"`
}
