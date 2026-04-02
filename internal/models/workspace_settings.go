package models

import "encoding/json"

type WorkspaceSettings struct {
	Context *WorkspaceContext `json:"context,omitempty"`
}

type WorkspaceContext struct {
	Repositories []WorkspaceRepository `json:"repositories,omitempty"`
	Paths        *WorkspacePaths       `json:"paths,omitempty"`
	Commands     *WorkspaceCommands    `json:"commands,omitempty"`
	Stack        *WorkspaceStack       `json:"stack,omitempty"`
	Deployment   *WorkspaceDeployment  `json:"deployment,omitempty"`
	Assumptions  []string              `json:"assumptions,omitempty"`
}

type WorkspaceRepository struct {
	Name string `json:"name,omitempty"`
	Role string `json:"role,omitempty"`
	Path string `json:"path,omitempty"`
	Repo string `json:"repo,omitempty"`
}

type WorkspacePaths struct {
	Root        string `json:"root,omitempty"`
	DocsRepo    string `json:"docs_repo,omitempty"`
	Web         string `json:"web,omitempty"`
	Server      string `json:"server,omitempty"`
	Skills      string `json:"skills,omitempty"`
	Config      string `json:"config,omitempty"`
	InstallRoot string `json:"install_root,omitempty"`
}

type WorkspaceCommands struct {
	Setup  string `json:"setup,omitempty"`
	Build  string `json:"build,omitempty"`
	Test   string `json:"test,omitempty"`
	Lint   string `json:"lint,omitempty"`
	Format string `json:"format,omitempty"`
	Dev    string `json:"dev,omitempty"`
	Start  string `json:"start,omitempty"`
	Web    string `json:"web,omitempty"`
}

type WorkspaceStack struct {
	Languages       []string `json:"languages,omitempty"`
	Frameworks      []string `json:"frameworks,omitempty"`
	PackageManagers []string `json:"package_managers,omitempty"`
}

type WorkspaceDeployment struct {
	Mode    string `json:"mode,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Host    string `json:"host,omitempty"`
}

func ParseWorkspaceSettings(raw string) (*WorkspaceSettings, error) {
	if raw == "" {
		return &WorkspaceSettings{}, nil
	}

	var settings WorkspaceSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return nil, err
	}
	return &settings, nil
}

func SerializeWorkspaceSettings(settings *WorkspaceSettings) (string, error) {
	if settings == nil {
		settings = &WorkspaceSettings{}
	}

	payload, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func ExtractWorkspaceContext(raw string) *WorkspaceContext {
	settings, err := ParseWorkspaceSettings(raw)
	if err != nil {
		return nil
	}
	return settings.Context
}

func (w *Workspace) HydrateDerivedFields() {
	if w == nil {
		return
	}
	w.Context = ExtractWorkspaceContext(w.Settings)
}
