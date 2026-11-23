package storage

// EnrollmentRejection is a stable, non-sensitive enrollment protocol code.
type EnrollmentRejection string

const (
	PeeringKeyUnavailable       EnrollmentRejection = "peering_key_unavailable"
	PeeringKeyExpired           EnrollmentRejection = "peering_key_expired"
	PeeringKeyInvalid           EnrollmentRejection = "peering_key_invalid"
	EnrollmentConflict          EnrollmentRejection = "enrollment_conflict"
	InstallationAlreadyEnrolled EnrollmentRejection = "installation_already_enrolled"
)

func (r EnrollmentRejection) Error() string { return string(r) }

// Message returns local text for recognized protocol codes.
func (r EnrollmentRejection) Message() string {
	switch r {
	case PeeringKeyUnavailable:
		return "No active peering key. Generate a new key on the server; restarting the server clears existing keys"
	case PeeringKeyExpired:
		return "Peering key expired. Generate a new key on the server and copy it again"
	case PeeringKeyInvalid:
		return "Peering key is invalid or was replaced. Copy the current key from the intended server"
	case EnrollmentConflict:
		return "Enrollment request is already bound to different credentials; retry the original request"
	case InstallationAlreadyEnrolled:
		return "This installation is already enrolled. Use its existing connection or recover it through the server administrator"
	default:
		return ""
	}
}
