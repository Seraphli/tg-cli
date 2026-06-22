package helpers

import (
	"encoding/json"
	"fmt"

	"github.com/Seraphli/tg-cli/cmd/stores"
)

// PermDecision holds the resolved outcome of a PermissionRequest.
// Used as the return value of DoDecidePerm and related helpers.
type PermDecision struct {
	Behavior           string          `json:"behavior"`
	Message            string          `json:"message,omitempty"`
	UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
}

// WritePendingAnswer pushes an answer event to the wait store.
// The old file-IO variant is superseded; callers must now pass the wait store.
func WritePendingAnswer(waitStore *stores.PendingWaitStore, uuid string, ccOutput json.RawMessage) {
	waitStore.Push(uuid, stores.WaitEvent{Type: "answer", Output: ccOutput})
}

// BuildAskCCOutput builds CC output for AskUserQuestion.
func BuildAskCCOutput(payload json.RawMessage, answers map[string]string) json.RawMessage {
	var p map[string]interface{}
	json.Unmarshal(payload, &p)
	toolInput, _ := p["tool_input"].(map[string]interface{})
	questions, _ := toolInput["questions"].([]interface{})
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior": "allow",
				"updatedInput": map[string]interface{}{
					"questions": questions,
					"answers":   answers,
				},
			},
		},
	}
	result, _ := json.Marshal(output)
	return result
}

// BuildPermCCOutput builds CC output for PermissionRequest.
func BuildPermCCOutput(decision string, message string, updatedPerms []interface{}) json.RawMessage {
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior": decision,
			},
		},
	}
	decisionMap := output["hookSpecificOutput"].(map[string]interface{})["decision"].(map[string]interface{})
	if message != "" {
		decisionMap["message"] = message
	}
	if updatedPerms != nil {
		decisionMap["updatedPermissions"] = updatedPerms
	}
	result, _ := json.Marshal(output)
	return result
}

// ResolvePermission resolves a PermissionRequest decision using raw suggestions JSON.
// Callers that used to read from PendingPermStore must now pass snap.PermSuggestions directly.
func ResolvePermission(suggestions json.RawMessage, decision string) (PermDecision, error) {
	return resolvePermDecision(suggestions, decision)
}

// resolvePermissionFromSnap resolves a PermissionRequest decision from an EntrySnapshot.
func resolvePermissionFromSnap(snap stores.EntrySnapshot, decision string) (PermDecision, error) {
	return resolvePermDecision(snap.PermSuggestions, decision)
}

// resolvePermDecision computes a PermDecision for a given decision string and suggestions JSON.
func resolvePermDecision(suggestions json.RawMessage, decision string) (PermDecision, error) {
	d := PermDecision{}
	switch {
	case decision == "allow":
		d.Behavior = "allow"
	case decision == "deny":
		d.Behavior = "deny"
	case decision == "sAll":
		d.Behavior = "allow"
		var sugArr []json.RawMessage
		json.Unmarshal(suggestions, &sugArr)
		d.UpdatedPermissions, _ = json.Marshal(sugArr)
	case len(decision) > 1 && decision[0] == 's':
		idx := 0
		for _, ch := range decision[1:] {
			if ch < '0' || ch > '9' {
				return d, fmt.Errorf("invalid suggestion index")
			}
			idx = idx*10 + int(ch-'0')
		}
		d.Behavior = "allow"
		var sugArr []json.RawMessage
		json.Unmarshal(suggestions, &sugArr)
		if idx < len(sugArr) {
			d.UpdatedPermissions, _ = json.Marshal([]json.RawMessage{sugArr[idx]})
		}
	default:
		return d, fmt.Errorf("unknown decision: %s", decision)
	}
	return d, nil
}
