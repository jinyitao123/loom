package stdlib

import (
	"context"
	"encoding/json"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
)

// SaveSession persists the conversation messages for a session.
func SaveSession(store loom.Store, sessionID string, msgs []contract.Message) error {
	data, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	return store.Put(context.Background(), "session", sessionID, data)
}

// LoadSession restores conversation messages for a session.
// Returns nil, nil if the session does not exist (not an error).
func LoadSession(store loom.Store, sessionID string) ([]contract.Message, error) {
	data, err := store.Get(context.Background(), "session", sessionID)
	if err != nil {
		return nil, nil // no session = empty history
	}
	var msgs []contract.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}
