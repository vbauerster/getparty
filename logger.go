package getparty

import (
	"fmt"
	"log"
)

const (
	DEBUG = iota
	INFO
	WARN
	ERRO
	LEVELS
)

func (cmd *Cmd) initLoggers() {
	out := cmd.getOut()
	cmd.loggers[DEBUG] = log.New(cmd.getErr(), fmt.Sprintf("[%s] ", cmdName), log.LstdFlags)
	cmd.loggers[INFO] = log.New(out, "[INFO] ", log.LstdFlags)
	cmd.loggers[WARN] = log.New(out, "[WARN] ", log.LstdFlags)
	cmd.loggers[ERRO] = log.New(out, "[ERRO] ", log.LstdFlags)
}
