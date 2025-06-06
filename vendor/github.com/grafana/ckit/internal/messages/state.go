package messages

import (
	"fmt"

	"github.com/grafana/ckit/internal/lamport"
	"github.com/grafana/ckit/peer"
)

// State represents a State change broadcast from a node.
type State struct {
	// Name of the node this state change is for.
	NodeName string
	// New State of the node.
	NewState peer.State
	// Time the state was generated.
	Time lamport.Time
}

// String returns the string representation of the State message.
func (s State) String() string {
	return fmt.Sprintf("%s @%d: %s", s.NodeName, s.Time, s.NewState)
}

var _ Message = (*State)(nil)

// Type implements Message.
func (s *State) Type() Type { return TypeState }

// Name implements Message.
func (s *State) Name() string { return s.NodeName }

// Cache implements Message.
func (s *State) Cache() bool { return true }
