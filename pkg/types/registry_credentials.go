package types

// RegistryCredentials is a credential pair used for basic auth
// The json tags name the keys of a Docker auth config. Decoding was already
// case-insensitive on the field names, so they do not change what TransformAuth
// accepts.
type RegistryCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"` // usually a token rather than an actual password
}
