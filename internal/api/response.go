// Package api provides common HTTP response utilities.
package api

import (
	"encoding/json"
	"net/http"
)

// Response is a generic HTTP API response structure.
type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// JSONError sends a JSON formatted error response.
func JSONError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(Response{Success: false, Message: message})
}

// JSONOK sends a JSON formatted success response.
func JSONOK(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
