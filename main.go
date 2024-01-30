package main

import (
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	awsBrooker "github.com/itslearninggermany/itswizard_m_awsbrooker"
	itswizard_basic "github.com/itslearninggermany/itswizard_m_basic"
	"github.com/jinzhu/gorm"
	"log"
	"strings"
	"sync"
	"time"
)

var (
	cwl                 *cloudwatchlogs.CloudWatchLogs
	logGroupName        = "LastNameErrorHandler"
	logStreamName       = ""
	sequenceToken       = ""
	finishedMainRoutine bool
	finishedLogger      bool
	allDatabases        map[string]*gorm.DB
	dbWebserver         *gorm.DB
	dbClient            *gorm.DB
)

type logger struct {
	arr  *[]string
	lock *sync.Mutex
}

type LogStore struct {
	Kontext    string
	LogContent interface{}
}

type LogEintragError struct {
	Error       bool
	ErrorString string
}

type LogEintragInformation struct {
	Information string
}

func init() {

	//Init Logging
	sess, err := session.NewSessionWithOptions(session.Options{
		Config: aws.Config{
			Region: aws.String("eu-central-1"), // london
		},
	})

	if err != nil {
		panic(err)
	}

	cwl = cloudwatchlogs.New(sess)

	err = ensureLogGroupExists(logGroupName)
	if err != nil {
		panic(err)
	}
	// End Init Logging

	// Datenbanken einlesen
	var databaseConfig []itswizard_basic.DatabaseConfig
	b, _ := awsBrooker.DownloadFileFromBucket("brooker", "admin/databaseconfig.json")
	err = json.Unmarshal(b, &databaseConfig)
	if err != nil {
		panic("Error by reading database file " + err.Error())
		return
	}
	allDatabases = make(map[string]*gorm.DB)
	for i := 0; i < len(databaseConfig); i++ {
		database, err := gorm.Open(databaseConfig[i].Dialect, databaseConfig[i].Username+":"+databaseConfig[i].Password+"@tcp("+databaseConfig[i].Host+")/"+databaseConfig[i].NameOrCID+"?charset=utf8&parseTime=True&loc=Local")
		if err != nil {
			panic(err)
		}
		allDatabases[databaseConfig[i].NameOrCID] = database
	}
	dbWebserver = allDatabases["Webserver"]
	dbClient = allDatabases["Client"]
	// Datenbank einlesen ende
}

func main() {
	queue := []string{}
	lock := sync.Mutex{}
	logger := InitLog(&queue, &lock)

	go MainProgram(logger)

	go processQueue(&queue, &lock)

	// to stop the code from exiting
	for {
		if finishedMainRoutine {
			if finishedLogger {
				return
			}
		}
	}
}

func InitLog(arr *[]string, lock *sync.Mutex) *logger {
	x := new(logger)
	x.arr = arr
	x.lock = lock
	return x
}

func (p *logger) log(err error, Kontext string, information string) {
	var x interface{}
	if err != nil {
		x = LogEintragError{
			Error:       true,
			ErrorString: err.Error(),
		}
	} else {
		x = LogEintragInformation{Information: information}
	}
	tmp := LogStore{
		Kontext:    Kontext,
		LogContent: x,
	}
	b, err := json.Marshal(tmp)
	if err != nil {
		panic(err)
	}
	logData(string(b), p.arr, p.lock)
}

func logData(log string, arr *[]string, lock *sync.Mutex) {
	lock.Lock()
	*arr = append(*arr, log)
	lock.Unlock()
}

//func MainProgramm(arr *[]string, lock *sync.Mutex) {

// ensureLogGroupExists first checks if the log group exists,
// if it doesn't it will create one.
func ensureLogGroupExists(name string) error {
	resp, err := cwl.DescribeLogGroups(&cloudwatchlogs.DescribeLogGroupsInput{})
	if err != nil {
		return err
	}

	for _, logGroup := range resp.LogGroups {
		if *logGroup.LogGroupName == name {
			return nil
		}
	}

	_, err = cwl.CreateLogGroup(&cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: &name,
	})
	if err != nil {
		return err
	}

	_, err = cwl.PutRetentionPolicy(&cloudwatchlogs.PutRetentionPolicyInput{
		RetentionInDays: aws.Int64(14),
		LogGroupName:    &name,
	})

	return err
}

// createLogStream will make a new logStream with a random uuid as its name.
func createLogStream() error {
	name := logStreamName
	if logStreamName == "" {
		name = uuid.New().String()
	}

	_, err := cwl.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  &logGroupName,
		LogStreamName: &name,
	})

	logStreamName = name

	return err
}

// processQueue will process the log queue
func processQueue(queue *[]string, lock *sync.Mutex) {
	var logQueue []*cloudwatchlogs.InputLogEvent

	for {
		lock.Lock()
		finishedLogger = false
		if len(*queue) > 0 {

			for _, item := range *queue {
				logQueue = append(logQueue, &cloudwatchlogs.InputLogEvent{
					Message:   &item,
					Timestamp: aws.Int64(time.Now().UnixNano() / int64(time.Millisecond)),
				})
				break
			}
			tmp := *queue
			tmp = tmp[1:]
			*queue = tmp
		}

		lock.Unlock()

		fmt.Println(len(logQueue))
		if len(logQueue) > 0 {
			input := cloudwatchlogs.PutLogEventsInput{
				LogEvents:    logQueue,
				LogGroupName: &logGroupName,
			}

			if sequenceToken == "" {
				err := createLogStream()
				if err != nil {
					panic(err)
				}
			} else {
				input = *input.SetSequenceToken(sequenceToken)
			}

			input = *input.SetLogStreamName(logStreamName)

			resp, err := cwl.PutLogEvents(&input)
			if err != nil {
				log.Println(err)
			}

			if resp != nil {
				sequenceToken = *resp.NextSequenceToken
			}

			logQueue = []*cloudwatchlogs.InputLogEvent{}
		}
		lock.Lock()
		if len(*queue) == 0 && finishedMainRoutine {
			finishedLogger = true
		}
		lock.Unlock()

		time.Sleep(time.Second * 5)
		fmt.Println("Sleeping 5 Seconds")
	}

}

func MainProgram(logger *logger) {
	logger.log(nil, "Start Service", "")

	//Alle Univnetion installationen

	var univentionservice []itswizard_basic.UniventionService
	err := allDatabases["Client"].Find(&univentionservice).Error
	if err != nil {
		logger.log(err, "Reading Univentionservice", "")

		return
	}

	for _, v := range univentionservice {
		fmt.Println(v.InsitutionID)

		var users []itswizard_basic.UniventionPerson
		tmp := fmt.Sprint(v.InsitutionID)
		allDatabases[tmp].Where("error = ?", true).Find(&users)

		for _, user := range users {
			if strings.Contains(user.Errorstring, "Part or all the supplied data was detected as invalid by itslearning. Missing field: 'LastName'") {
				fmt.Println("Lastname Error found", user.Username)
				logger.log(err, fmt.Sprint("Lastname Error found", user.Username), fmt.Sprint(v.InsitutionID))
				user.Error = false
				user.ToUpdate = false
				user.ToDelete = false
				user.ToImport = true
				user.Errorstring = ""
				user.Success = false
				allDatabases[tmp].Save(&user)
			}

			if strings.Contains(user.Errorstring, "The ‘sourcedId’ identifier can’t be empty") {
				fmt.Println("Source ID Empty", user.Username)
				logger.log(err, fmt.Sprint("Source ID Empty", user.Username), fmt.Sprint(v.InsitutionID))
				allDatabases[tmp].Delete(&user)
				time.Sleep(time.Second)
			}
		}
	}
	finishedMainRoutine = true
}
