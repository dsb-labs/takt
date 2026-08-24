package service

import (
	"time"
)

// The Errors interface describes how the service reads why workloads are not
// converging.
//
// Recording the errors is the reconciler's job rather than this one's: a converge
// failure is only observable during the pass that hits it, and the reconciler is what
// runs the passes. The service reads whatever the last pass left behind.
type Errors interface {
	// LastError should report why the last converge pass over a workload failed and
	// when, reporting false when the workload's last pass succeeded or none has run.
	LastError(workload string) (string, time.Time, bool)
}

// lastError returns why a workload's last converge pass failed, reporting zero values
// for one that is converging.
func (s *WorkloadService) lastError(workload string) (string, time.Time) {
	if s.errors == nil {
		return "", time.Time{}
	}

	message, at, ok := s.errors.LastError(workload)
	if !ok {
		return "", time.Time{}
	}

	return message, at
}
