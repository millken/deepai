//go:build !unix

package chat

// watchStopResume is a no-op off Unix: job-control stop signals do not
// exist there. See signal_stop_resume_unix.go for the real lifecycle.
func (r *ChatRepl) watchStopResume(done <-chan struct{}) {}
