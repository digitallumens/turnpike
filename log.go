package turnpike

import (
	"github.com/digitallumens/logrus-stack"
	logrus "github.com/sirupsen/logrus"
)

var log = logrus.New()

// setup logger for package
func init() {
	// Output using text formatter with full timestamps
	log.Formatter = &logrus.JSONFormatter{}
	// Only log the info severity or above.
	log.Level = logrus.InfoLevel

	callerLevels := []logrus.Level{logrus.PanicLevel, logrus.FatalLevel, logrus.ErrorLevel}
	stackLevels := []logrus.Level{}

	log.Hooks.Add(logrus_stack.NewHook(callerLevels, stackLevels))
}

func logErr(err error) error {
	if err == nil {
		return nil
	}
	log.Errorf("%s", err)
	return err
}
