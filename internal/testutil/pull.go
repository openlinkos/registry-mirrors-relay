package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Puller is a minimal pull client: it pulls index → manifest → config → layers in turn and verifies
// every digest.
type Puller struct {
	Base   string // e.g. http://127.0.0.1:12345
	Host   string // Host header of the requests
	Client *http.Client
	Header http.Header // extra headers sent with every request
}

func (p *Puller) get(path, accept string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, p.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Host = p.Host
	for k, v := range p.Header {
		req.Header[k] = v
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	c := p.Client
	if c == nil {
		c = http.DefaultClient
	}
	return c.Do(req)
}

func (p *Puller) fetch(path, accept, want string) ([]byte, error) {
	resp, err := p.get(path, accept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, b)
	}
	if want != "" && Digest(b) != want {
		return nil, fmt.Errorf("%s: digest %s, want %s", path, Digest(b), want)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" && d != Digest(b) {
		return nil, fmt.Errorf("%s: Docker-Content-Digest %s does not match the content", path, d)
	}
	return b, nil
}

// Pull pulls every platform of name:ref and returns the index digest and the number of bytes pulled.
func (p *Puller) Pull(name, ref string) (string, int64, error) {
	idx, err := p.fetch("/v2/"+name+"/manifests/"+ref, MediaIndex+", "+MediaManifest, "")
	if err != nil {
		return "", 0, err
	}
	total := int64(len(idx))
	var index struct {
		Manifests []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(idx, &index); err != nil {
		return "", 0, err
	}
	for _, m := range index.Manifests {
		mb, err := p.fetch("/v2/"+name+"/manifests/"+m.Digest, MediaManifest, m.Digest)
		if err != nil {
			return "", 0, err
		}
		total += int64(len(mb))
		var man struct {
			Config descriptor   `json:"config"`
			Layers []descriptor `json:"layers"`
		}
		if err := json.Unmarshal(mb, &man); err != nil {
			return "", 0, err
		}
		for _, d := range append([]descriptor{man.Config}, man.Layers...) {
			b, err := p.fetch("/v2/"+name+"/blobs/"+d.Digest, "", d.Digest)
			if err != nil {
				return "", 0, err
			}
			total += int64(len(b))
		}
	}
	return Digest(idx), total, nil
}
