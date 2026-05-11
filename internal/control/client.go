package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
)

// Call dials the control socket, writes a single JSON request, reads the
// JSON response, and returns it.
func Call(socketPath string, req Request) (*Response, error) {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", socketPath, err)
	}
	defer c.Close()

	out, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	if _, err := c.Write(out); err != nil {
		return nil, err
	}

	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w (raw=%s)", err, string(line))
	}
	return &resp, nil
}
