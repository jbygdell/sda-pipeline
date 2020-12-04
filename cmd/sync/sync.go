// The sync service accepts messages for files that have been uploaded
// and creates a copy to the destination s3 backend
package main

import (
	"encoding/json"
	"io"
	"os"

	"sda-pipeline/internal/broker"
	"sda-pipeline/internal/config"
	"sda-pipeline/internal/database"
	"sda-pipeline/internal/storage"

	log "github.com/sirupsen/logrus"
	"github.com/xeipuuv/gojsonschema"
)

// sync struct that holds the json message data
type sync struct {
	Type               string      `json:"type"`
	User               string      `json:"user"`
	Filepath           string      `json:"filepath"`
	AccessionID        string      `json:"accession_id"`
	DecryptedChecksums []checksums `json:"decrypted_checksums"`
}

// Checksums is struct for the checksum type and value
type checksums struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Completed is struct holding the full message data
type completed struct {
	User               string      `json:"user"`
	Filepath           string      `json:"filepath"`
	AccessionID        string      `json:"accession_id"`
	DecryptedChecksums []checksums `json:"decrypted_checksums"`
}

func main() {
	conf, err := config.NewConfig("sync")
	if err != nil {
		log.Fatal(err)
	}
	mq, err := broker.NewMQ(conf.Broker)
	if err != nil {
		log.Fatal(err)
	}
	db, err := database.NewDB(conf.Database)
	if err != nil {
		log.Fatal(err)
	}
	backup, err := storage.NewBackend(conf.Backup)
	if err != nil {
		log.Fatal(err)

	}
	inbox, err := storage.NewBackend(conf.Inbox)
	if err != nil {
		log.Fatal(err)

	}

	defer mq.Channel.Close()
	defer mq.Connection.Close()
	defer db.Close()

	go func() {
		connError := mq.ConnectionWatcher()
		log.Error(connError)
		os.Exit(1)
	}()

	forever := make(chan bool)

	log.Info("starting sync service")
	var message sync

	go func() {
		messages, err := mq.GetMessages(conf.Broker.Queue)
		if err != nil {
			log.Fatal(err)
		}
		for delivered := range messages {
			log.Debugf("received a message: %s", delivered.Body)
			res, err := validateJSON(conf.SchemasPath, delivered.Body)
			if err != nil {
				log.Errorf("josn error: %v", err)
				// Nack message so the server gets notified that something is wrong but don't requeue the message
				if e := delivered.Nack(false, false); e != nil {
					log.Errorln("failed to Nack message, reason: ", e)
				}

				// Send the message to an error queue so it can be analyzed.
				if e := mq.SendJSONError(&delivered, err.Error(), conf.Broker); e != nil {
					log.Error("failed to publish message, reason: ", err)
				}
				// Restart on new message
				continue
			}
			if !res.Valid() {
				log.Errorf("result.error: %v", res.Errors())
				log.Error("Validation failed")
				// Nack message so the server gets notified that something is wrong but don't requeue the message
				if e := delivered.Nack(false, false); e != nil {
					log.Errorln("failed to Nack message, reason: ", e)
				}

				// Send the message to an error queue so it can be analyzed.
				if e := mq.SendJSONError(&delivered, err.Error(), conf.Broker); e != nil {
					log.Error("failed to publish message, reason: ", res.Errors())
				}
				// Restart on new message
				continue
			}

			// we unmarshal the message in the validation step so this is safe to do
			_ = json.Unmarshal(delivered.Body, &message)

			// Extract the sha256 from the message and use it for the database
			var checksumSha256 string
			for _, checksum := range message.DecryptedChecksums {
				if checksum.Type == "sha256" {
					checksumSha256 = checksum.Value
				}
			}

			c := completed{
				User:               message.User,
				Filepath:           message.Filepath,
				AccessionID:        message.AccessionID,
				DecryptedChecksums: message.DecryptedChecksums,
			}

			completeMsg, _ := json.Marshal(&c)

			res, err = validateJSON(conf.SchemasPath, completeMsg)
			if err != nil {
				log.Errorf("json error: %v", err)
				// Nack message so the server gets notified that something is wrong but don't requeue the message
				if e := delivered.Nack(false, false); e != nil {
					log.Errorln("failed to Nack message, reason: ", e)
				}
				// Send the message to an error queue so it can be analyzed.
				if e := mq.SendJSONError(&delivered, err.Error(), conf.Broker); e != nil {
					log.Error("failed to publish message, reason: ", err)
				}
				// Restart on new message
				continue
			}
			if !res.Valid() {
				log.Errorf("result.error: %v", res.Errors())
				log.Error("Validation failed")
				// Nack message so the server gets notified that something is wrong but don't requeue the message
				if e := delivered.Nack(false, false); e != nil {
					log.Errorln("failed to Nack message, reason: ", e)
				}
				// Send the message to an error queue so it can be analyzed.
				if e := mq.SendJSONError(&delivered, err.Error(), conf.Broker); e != nil {
					log.Error("failed to publish message, reason: ", err)
				}
				// Restart on new message
				continue
			}

			log.Debug("Sync initiated")
			file, err := inbox.NewFileReader(message.Filepath)
			if err != nil {
				log.Errorf("Failed to open file: %s, reason: %v", message.Filepath, err)
				continue
			}

			dest, err := backup.NewFileWriter(message.Filepath)
			if err != nil {
				log.Errorf("Failed to create file, reason: %v", err)
				continue
			}

			_, err = io.Copy(dest, file)
			if err != nil {
				log.Fatal(err)
			}

			file.Close()
			dest.Close()
			log.Debugln("Sync completed")

			log.Debug("Mark ready")
			if err := db.MarkReady(message.AccessionID, message.User, message.Filepath, checksumSha256); err != nil {
				log.Errorf("MarkReady failed, reason: %v", err)
				// nack the message but requeue until we fixed the SQL retry.
				if e := delivered.Nack(false, true); e != nil {
					log.Errorln("failed to Nack message, reason: ", e)
				}
				continue
				// this should be handled by the SQL retry mechanism
			}

			if err := mq.SendMessage(delivered.CorrelationId, conf.Broker.Exchange, conf.Broker.RoutingKey, conf.Broker.Durable, completeMsg); err != nil {
				// TODO fix resend mechanism
				log.Errorln("We need to fix this resend stuff ...")
			}

			if err := delivered.Ack(false); err != nil {
				log.Errorf("failed to ack message for reason: %v", err)
			}
		}
	}()

	<-forever

}

// Validate the JSON in a received message
func validateJSON(schemasPath string, body []byte) (*gojsonschema.Result, error) {
	message := make(map[string]interface{})
	err := json.Unmarshal(body, &message)
	if err != nil {
		return nil, err
	}

	var schema gojsonschema.JSONLoader

	_, ok := message["type"]
	if ok {
		schema = gojsonschema.NewReferenceLoader(schemasPath + "ingestion-accession.json")
	} else {
		schema = gojsonschema.NewReferenceLoader(schemasPath + "ingestion-completion.json")
	}
	res, err := gojsonschema.Validate(schema, gojsonschema.NewBytesLoader(body))
	return res, err
}
