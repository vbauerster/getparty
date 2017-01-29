package main

type temporary interface {
	Temporary() bool
}

func isTemporary(err error) bool {
	te, ok := err.(temporary)
	return ok && te.Temporary()
}
