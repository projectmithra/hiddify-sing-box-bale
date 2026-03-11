package option

import "github.com/sagernet/sing/common/json/badoption"

type V2RayBaleOptions struct {
WorkerURL      string               `json:"worker_url,omitempty"`
WorkerHost     string               `json:"worker_host,omitempty"`
Origin         string               `json:"origin,omitempty"`
AcceptLanguage string               `json:"accept_language,omitempty"`
Path           string               `json:"path,omitempty"`
Headers        badoption.HTTPHeader `json:"headers,omitempty"`
}
