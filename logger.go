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
	cmd.loggers[DEBUG] = log.New(cmd.Err, fmt.Sprintf("[%s] ", cmdName), log.LstdFlags)
	cmd.loggers[INFO] = log.New(cmd.Out, "[INFO] ", log.LstdFlags)
	cmd.loggers[WARN] = log.New(cmd.Out, "[WARN] ", log.LstdFlags)
	cmd.loggers[ERRO] = log.New(cmd.Out, "[ERRO] ", log.LstdFlags)
}
