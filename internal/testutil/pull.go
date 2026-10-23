package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Puller 是一个最小的拉取客户端：沿 index → manifest → config → 层一路拉下来，校验每个 digest。
type Puller struct {
	Base   string // 如 http://127.0.0.1:12345
	Host   string // 请求的 Host 头
	Client *http.Client
	Header http.Header // 每个请求额外带的头
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
		return nil, fmt.Errorf("%s：状态 %d：%s", path, resp.StatusCode, b)
	}
	if want != "" && Digest(b) != want {
		return nil, fmt.Errorf("%s：digest %s，期望 %s", path, Digest(b), want)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" && d != Digest(b) {
		return nil, fmt.Errorf("%s：Docker-Content-Digest %s 和内容不符", path, d)
	}
	return b, nil
}

// Pull 拉取 name:ref 的所有平台，返回 index 的 digest 和拉到的字节数。
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
