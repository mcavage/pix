package uat

type HostFailureError struct {
	Reason string
}

func (e *HostFailureError) Error() string {
	return e.Reason
}

func IsHostFailure(err error) bool {
	_, ok := err.(*HostFailureError)
	return ok
}
