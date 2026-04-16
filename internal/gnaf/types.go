package gnaf

// Address is the normalised representation returned by all API endpoints.
type Address struct {
	PID         string  `json:"pid"`
	FullAddress string  `json:"full_address"`
	Number      string  `json:"number"`
	StreetName  string  `json:"street_name"`
	StreetType  string  `json:"street_type"`
	Suburb      string  `json:"suburb"`
	State       string  `json:"state"`
	Postcode    string  `json:"postcode"`
	Latitude    float64 `json:"latitude,omitempty"`
	Longitude   float64 `json:"longitude,omitempty"`
}

// ValidateRequest is the structured input for the validate endpoint.
type ValidateRequest struct {
	Number     string `json:"number"`
	StreetName string `json:"street_name"`
	StreetType string `json:"street_type"`
	Suburb     string `json:"suburb"`
	State      string `json:"state"`
	Postcode   string `json:"postcode"`
}

// ValidateResponse wraps the best-matched address with a confidence score.
type ValidateResponse struct {
	Match      *Address `json:"match"`
	Confidence float64  `json:"confidence"` // 0.0–1.0
	Matched    bool     `json:"matched"`
}

// SearchResponse wraps a list of address results with pagination metadata.
type SearchResponse struct {
	Results []Address `json:"results"`
	Total   int       `json:"total"`
	Query   string    `json:"query"`
}
