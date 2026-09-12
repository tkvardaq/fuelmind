package ask

import "encoding/json"

// parseIssueMessages pulls the human-readable messages out of the score's
// issues_json. They are mart-computed text, safe to show and to quote.
func parseIssueMessages(issuesJSON string) []string {
	if issuesJSON == "" {
		return nil
	}
	var issues []struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(issuesJSON), &issues); err != nil {
		return nil
	}
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		if i.Message != "" {
			out = append(out, i.Message)
		}
	}
	return out
}
