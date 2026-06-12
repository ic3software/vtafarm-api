package setup

import (
	"bytes"
	"text/template"

	"github.com/ic3software/vtafarm-api/internal/model"
)

var vtaSetupTmpl = template.Must(template.New("vta-setup").Parse(`config_path = "config.toml"
data_dir    = "data/vta"
vta_name    = "{{ .VtaName }}"
public_url  = "{{ .PublicURL }}"

[services]
rest    = true
didcomm = true

[server]
host = "0.0.0.0"
port = 8100

[log]
level  = "info"
format = "text"

[secrets]
backend = "plaintext"

[messaging]
kind = "existing"
did  = "{{ .MediatorDid }}"

[vta_did]
kind               = "create_webvh"
url                = "{{ .VtaDidUrl }}"
portable           = {{ .Portable }}
pre_rotation_count = {{ .PreRotationCount }}
`))

type vtaSetupData struct {
	VtaName          string
	PublicURL        string
	MediatorDid      string
	VtaDidUrl        string
	Portable         bool
	PreRotationCount int
}

func RenderVtaSetupTOML(s *model.SetupSession) (string, error) {
	var buf bytes.Buffer
	err := vtaSetupTmpl.Execute(&buf, vtaSetupData{
		VtaName:          s.VtaName,
		PublicURL:        s.PublicURL(),
		MediatorDid:      s.MediatorDid,
		VtaDidUrl:        s.VtaDidUrl,
		Portable:         s.Portable,
		PreRotationCount: s.PreRotationCount,
	})
	return buf.String(), err
}
