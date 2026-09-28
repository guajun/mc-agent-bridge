// Package version is the single source of Go runtime version metadata.
package version

// Version is the Go CLI/daemon product version.
const Version = "0.5.0"

// Protocol is the control protocol version this runtime speaks.
const Protocol = 1

// ModMinVersion is the oldest mc-agent-interface-mod release that speaks the
// formal control protocol with the capability set this runtime expects.
const ModMinVersion = "0.8.0"

// UserAgent is used in protocol metadata, never for authentication.
const UserAgent = "mc-agent-go"
