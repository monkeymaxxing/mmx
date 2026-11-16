package modules

// Names the configuration loader reads.
var (
	// AccountRule accepts an owner or group: an account name, or an ID as an
	// integer.
	AccountRule = accountRule
	// KeyPaths lists an authorized_keys task's key files and key directories.
	KeyPaths = keyPaths
	// PermissionsModePattern is the form of a permissions_mode, such as 0644.
	PermissionsModePattern = modePattern
	// IsModuleEntry reports the file name of an executable module.
	IsModuleEntry = moduleEntry
)

func (f File) Action() string  { return f.action() }
func (a Account) Literal() any { return a.literal() }
