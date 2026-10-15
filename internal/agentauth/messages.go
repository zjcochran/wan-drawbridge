// SPDX-License-Identifier: AGPL-3.0-or-later

package agentauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/hosts"
)

// Paths of the manager's agent API.
const (
	PathEnroll    = "/api/agent/v1/enroll"
	PathCommands  = "/api/agent/v1/commands"
	PathResults   = "/api/agent/v1/results"
	PathInventory = "/api/agent/v1/inventory"
	PathRotate    = "/api/agent/v1/rotate"
)

// PollWait is the longest a poll for commands is held open: well below the
// 60-second read timeouts reverse proxies use by default.
const PollWait = 25 * time.Second

// Command types.
const (
	CmdStop    = "stop"
	CmdStart   = "start"
	CmdInspect = "inspect"
	CmdStats   = "stats"
	CmdRotate  = "rotate" // rotate the agent's key now
	CmdRekey   = "rekey"  // pin a new manager key
)

// Command is something the manager asks an agent to do. It is signed with the
// manager's key named by KeyID.
type Command struct {
	ID       string          `json:"id"`
	Host     int64           `json:"host"`
	Type     string          `json:"type"`
	Workload string          `json:"workload,omitempty"` // the workload ID
	Args     json.RawMessage `json:"args,omitempty"`
	Expires  int64           `json:"expires"`
	KeyID    string          `json:"key_id"`
	Sig      string          `json:"sig"`
}

func (c Command) text() []byte {
	return fmt.Appendf(nil, "command\n%s\n%d\n%s\n%s\n%s\n%d\n%s",
		c.ID, c.Host, c.Type, c.Workload, c.Args, c.Expires, c.KeyID)
}

// NewCommand makes a signed command with a random ID.
func NewCommand(k Key, keyID string, host int64, typ, workload string, args any, expires time.Time) (Command, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Command{}, err
	}
	c := Command{ID: hex.EncodeToString(id), Host: host, Type: typ, Workload: workload, Expires: expires.Unix(), KeyID: keyID}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return Command{}, err
		}
		c.Args = b
	}
	c.Sig = k.Sign(c.text())
	return c, nil
}

// Check verifies a command for host against the pinned manager key, and that
// it has not expired.
func (c Command) Check(pinned PublicKey, host int64, now time.Time) error {
	switch {
	case !pinned.Verify(c.text(), c.Sig):
		return fmt.Errorf("command %s: not signed by the pinned manager key", c.ID)
	case c.Host != host:
		return fmt.Errorf("command %s: for host %d, not this one (%d)", c.ID, c.Host, host)
	case now.Unix() > c.Expires:
		return fmt.Errorf("command %s: expired", c.ID)
	}
	return nil
}

// StopArgs are the arguments of a stop command.
type StopArgs struct {
	TimeoutSeconds int `json:"timeout_seconds"`
}

// StartArgs are the arguments of a start command.
type StartArgs struct {
	Restore []byte `json:"restore,omitempty"`
}

// RekeyArgs carry the manager's new key.
type RekeyArgs struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

// Result is an agent's answer to a command.
type Result struct {
	ID    string          `json:"id"`
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// InspectData is the data of an inspect result.
type InspectData struct {
	Running bool   `json:"running"`
	Health  string `json:"health"`
}

// StopData is the data of a stop result.
type StopData struct {
	Restore []byte `json:"restore,omitempty"`
}

// Counters are a workload's network counters.
type Counters struct {
	RX int64 `json:"rx"`
	TX int64 `json:"tx"`
}

// StatsData maps workload IDs to their counters; workloads without counters
// are left out.
type StatsData map[string]Counters

// EnrollRequest is an agent's first call.
type EnrollRequest struct {
	Token     string `json:"token"`
	PublicKey string `json:"public_key"`
	Hostname  string `json:"hostname"`
	Platform  string `json:"platform"`
	Version   string `json:"version"`
}

// EnrollResponse tells an enrolled agent who it is and which manager key to pin.
type EnrollResponse struct {
	HostID       int64  `json:"host_id"`
	Name         string `json:"name"`
	ManagerKeyID string `json:"manager_key_id"`
	ManagerKey   string `json:"manager_key"`
}

// Inventory is the full list of an agent's workloads.
type Inventory struct {
	Version   string           `json:"version"`
	Platform  string           `json:"platform"`
	Workloads []hosts.Workload `json:"workloads"`
}

// Poll is the manager's answer to a poll.
type Poll struct {
	Commands []Command `json:"commands"`
}

// RotateRequest gives the manager an agent's new key. The request is signed
// with the current key; Proof is the new key's signature over
// RotationProof, showing the agent holds it.
type RotateRequest struct {
	NewKey string `json:"new_key"`
	TS     int64  `json:"ts"`
	Proof  string `json:"proof"`
}
