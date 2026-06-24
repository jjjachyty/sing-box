package adapter

// UserEntry is a protocol-agnostic user representation for runtime updates.
type UserEntry struct {
	Name     string `json:"name"`
	UUID     string `json:"uuid"`
	Password string `json:"password"`
	AlterId  int    `json:"alter_id"`
	Flow     string `json:"flow"`
}

// UserUpdatableInbound is implemented by protocol inbounds that support
// runtime user list updates without restart.
type UserUpdatableInbound interface {
	Inbound
	UpdateUsers(users []UserEntry) error
}
