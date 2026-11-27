package desktopapp

type Options struct {
	ServerURL              string
	Database               string
	Project                string
	ProjectDir             string
	ConversationID         string
	Preferences            string
	WorkPreferences        string
	GatewayPreferencesPath string
	GatewayStateDir        string
	KeyEnv                 string
	EnrolledProfile        string
	State                  string
	RunID                  string
	ConfigPath             string
	IsSet                  func(string) bool
}
