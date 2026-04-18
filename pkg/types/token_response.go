package types

// TokenResponse is returned by the registry on successful authentication.
// ExpiresIn is the token lifetime in seconds, per the Docker registry token
// spec. When omitted by the registry it defaults to 60.
type TokenResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
}
