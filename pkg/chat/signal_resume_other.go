//go:build !unix

package chat

// watchResumeRefresh is a no-op off Unix: the SIGCONT lifecycle it watches
// does not exist there. See signal_resume_unix.go for the real watcher.
func (r *ChatRepl) watchResumeRefresh(done <-chan struct{}) {}
