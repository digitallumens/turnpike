package turnpike

import (
	// "errors"
	// "fmt"
	"github.com/op/go-logging"
	// "os"
)

var log = logging.MustGetLogger("turnpike")

const format = "%{color}[%{time} %{level}]%{color:reset}[%{module}-%{shortfile}] %{message}"

// // setup logger for package, writes to /dev/null by default
// func init() {
// 	if devNull, err := os.Create(os.DevNull); err != nil {
// 		panic("could not create logger: " + err.Error())
// 	} else if os.Getenv("DEBUG") != "" {
// 		log = glog.New(os.Stderr, "", logFlags)
// 	} else {
// 		log = glog.New(devNull, "", 0)
// 	}
// }
//
// // change log output to stderr
// func Debug() {
// 	log = glog.New(os.Stderr, "", logFlags)
// }
//
// func logErr(v ...interface{}) error {
// 	err := errors.New(fmt.Sprintln(v...))
// 	log.Infof(err)
// 	return err
// }

func logErr(err error) error {
	if err == nil {
		return nil
	}
	log.Errorff("%s", err)
	return err
}
