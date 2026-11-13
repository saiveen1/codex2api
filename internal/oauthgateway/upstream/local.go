package upstream

// Group is a local codex2api account group, not a remote admin endpoint.
type Group struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type ImportResult struct {
	Total      int     `json:"total"`
	Success    int     `json:"success"`
	Updated    int     `json:"updated"`
	Duplicate  int     `json:"duplicate"`
	Failed     int     `json:"failed"`
	CreatedIDs []int64 `json:"created_ids"`
}
