package worker

import "strings"

const defaultRegYYDSMailBaseURL = "https://maliapi.215.im"

func normalizeRegYYDSMailBaseURL(raw string) string {
	baseURL := strings.TrimSpace(raw)
	baseURL = strings.TrimRight(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return defaultRegYYDSMailBaseURL
	}
	return baseURL
}
