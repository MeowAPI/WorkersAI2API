package proxy

import "github.com/bogdanfinn/tls-client/profiles"

type userAgentTemplate struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	UserAgent   string `json:"user_agent"`
	Description string `json:"description"`
}

type clientPreset struct {
	userAgentTemplate
	Fingerprint string `json:"fingerprint"`
}

type fingerprintProfile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	UserAgent   string `json:"user_agent"`
	Description string `json:"description"`
	profile     profiles.ClientProfile
}

// App templates describe UA strings, not verified TLS fingerprints of those apps.
var userAgentTemplates = []userAgentTemplate{
	{"deepseek-harness", "DeepSeek Harness · UA 模板", "deepseek-harness/0.2.1-rc.2 (+https://github.com/deepseek-ai/deepseek-harness)", "绑定此 UA 与 Go 默认传输；尚无已核实的 DeepSeek Harness 专属指纹，不会叠加浏览器指纹。"},
	{"claude-code", "Claude Code · UA 模板", "claude-cli/2.1.0 (external, cli)", "绑定此 UA 与 Go 默认传输；尚无已核实的 Claude Code 专属指纹，不会叠加浏览器指纹。"},
}

var fingerprintProfiles = []fingerprintProfile{
	{
		ID: "chrome_152", Name: "Chrome 152",
		UserAgent:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
		Description: "绑定 Chrome 152 UA 与对应的 TLS ClientHello、HTTP/2 SETTINGS 和伪头顺序。",
		profile:     profiles.Chrome_152,
	},
	{
		ID: "firefox_147", Name: "Firefox 147",
		UserAgent:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:147.0) Gecko/20100101 Firefox/147.0",
		Description: "绑定 Firefox 147 UA 与对应的 TLS ClientHello、HTTP/2 SETTINGS 和伪头顺序。",
		profile:     profiles.Firefox_147,
	},
	{
		ID: "safari_ios_18_0", Name: "Safari · iOS 18",
		UserAgent:   "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
		Description: "绑定 iOS 18 Safari UA 与对应的 TLS ClientHello、HTTP/2 SETTINGS 和伪头顺序。",
		profile:     profiles.Safari_IOS_18_0,
	},
}

func findFingerprint(id string) *fingerprintProfile {
	for i := range fingerprintProfiles {
		if fingerprintProfiles[i].ID == id {
			return &fingerprintProfiles[i]
		}
	}
	return nil
}

func (a account) effectiveUserAgent() string {
	if a.UserAgent != "" {
		return a.UserAgent
	}
	if profile := findFingerprint(a.Fingerprint); profile != nil {
		return profile.UserAgent
	}
	return ""
}

func clientPresets() []clientPreset {
	presets := make([]clientPreset, 0, len(userAgentTemplates)+len(fingerprintProfiles))
	for _, template := range userAgentTemplates {
		presets = append(presets, clientPreset{userAgentTemplate: template})
	}
	for _, profile := range fingerprintProfiles {
		presets = append(presets, clientPreset{
			userAgentTemplate: userAgentTemplate{profile.ID, profile.Name, profile.UserAgent, profile.Description},
			Fingerprint:       profile.ID,
		})
	}
	return presets
}

func bindAccountFingerprint(a *account, patch accountUpdate) error {
	if a.Fingerprint == "" {
		return nil
	}
	profile := findFingerprint(a.Fingerprint)
	if profile == nil {
		return invalid("不支持的 TLS/HTTP2 指纹，请从内置配置中选择")
	}
	if patch.UserAgent != nil && a.UserAgent != "" && a.UserAgent != profile.UserAgent {
		if patch.Fingerprint != nil {
			return invalid("User-Agent 与指纹不匹配；请使用配套模板，或关闭指纹后自定义 UA")
		}
		// Editing only the UA switches the account to the custom/native mode.
		a.Fingerprint = ""
		return nil
	}
	a.UserAgent = profile.UserAgent
	return nil
}

func clientProfileCatalog() any {
	return struct {
		UserAgents   []userAgentTemplate  `json:"user_agents"`
		Fingerprints []fingerprintProfile `json:"fingerprints"`
		Presets      []clientPreset       `json:"presets"`
	}{userAgentTemplates, fingerprintProfiles, clientPresets()}
}
