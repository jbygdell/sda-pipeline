// The sync command accepts messages with accessionIDs for
// ingested files and copies them to the second storage.
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
)

// Sync struct that holds the json message data
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
	archive, err := storage.NewBackend(conf.Archive)
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

	log.Info("Starting sync service")
	var message sync

	go func() {
		messages, err := mq.GetMessages(conf.Broker.Queue)
		if err != nil {
			log.Fatal(err)
		}
		for delivered := range messages {
			log.Debugf("Received a message (corr-id: %s, message: %s)",
				delivered.CorrelationId,
				delivered.Body)

			err := mq.ValidateJSON(&delivered,
				"ingestion-accession",
				delivered.Body,
				&message)

			if err != nil {
				log.Errorf("Validation of incoming message failed "+
					"(corr-id: %s, error: %v)",
					delivered.CorrelationId,
					err)
				continue
			}

			// we unmarshal the message in the validation step so this is safe to do
			_ = json.Unmarshal(delivered.Body, &message)

			log.Infof("Received work (corr-id: %s, "+
				"filepath: %s, "+
				"user: %s, "+
				"accessionid: %s, "+
				"decryptedChecksums: %v)",
				delivered.CorrelationId,
				message.Filepath,
				message.User,
				message.AccessionID,
				message.DecryptedChecksums)

			// Extract the sha256 from the message and use it for the database
			var checksumSha256 string
			for _, checksum := range message.DecryptedChecksums {
				if checksum.Type == "sha256" {
					checksumSha256 = checksum.Value
				}
			}

			var filePath string
			var fileSize int
			if filePath, fileSize, err = db.GetArchived(message.User, message.Filepath, checksumSha256); err != nil {
				log.Errorf("GetArchived failed "+
					"(corr-id: %s, "+
					"filepath: %s, "+
					"user: %s, "+
					"accessionid: %s, "+
					"decryptedChecksums: %v, error: %v)",
					delivered.CorrelationId,
					message.Filepath,
					message.User,
					message.AccessionID,
					message.DecryptedChecksums,
					err)

				// nack the message but requeue until we fixed the SQL retry.
				if e := delivered.Nack(false, true); e != nil {
					log.Errorf("Failed to NAck because of GetArchived failed "+
						"(corr-id: %s, "+
						"filepath: %s, "+
						"user: %s, "+
						"accessionid: %s, "+
						"decryptedChecksums: %v, error: %v)",
						delivered.CorrelationId,
						message.Filepath,
						message.User,
						message.AccessionID,
						message.DecryptedChecksums,
						e)
				}
				continue
			}

			log.Info("Sync initiated")
			file, err := archive.NewFileReader(filePath)
			if err != nil {
				log.Errorf("Failed to open archived file "+
					"(corr-id: %s, "+
					"filepath: %s, "+
					"user: %s, "+
					"accessionid: %s, "+
					"decryptedChecksums: %v, error: %v)",
					delivered.CorrelationId,
					message.Filepath,
					message.User,
					message.AccessionID,
					message.DecryptedChecksums,
					err)

				//FIXME: should it retry?
				if e := delivered.Nack(false, true); e != nil {
					log.Errorf("Failed to NAck because of NewFileReader failed "+
						"(corr-id: %s, "+
						"filepath: %s, "+
						"user: %s, "+
						"accessionid: %s, "+
						"decryptedChecksums: %v, error: %v)",
						delivered.CorrelationId,
						message.Filepath,
						message.User,
						message.AccessionID,
						message.DecryptedChecksums,
						e)
				}
				continue
			}

			dest, err := backup.NewFileWriter(filePath)
			if err != nil {
				log.Errorf("Failed to write archived file "+
					"(corr-id: %s, "+
					"filepath: %s, "+
					"user: %s, "+
					"accessionid: %s, "+
					"decryptedChecksums: %v, error: %v)",
					delivered.CorrelationId,
					message.Filepath,
					message.User,
					message.AccessionID,
					message.DecryptedChecksums,
					err)

				//FIXME: should it retry?
				if e := delivered.Nack(false, true); e != nil {
					log.Errorf("Failed to NAck because of NewFileWriter failed "+
						"(corr-id: %s, "+
						"filepath: %s, "+
						"user: %s, "+
						"accessionid: %s, "+
						"decryptedChecksums: %v, error: %v)",
						delivered.CorrelationId,
						message.Filepath,
						message.User,
						message.AccessionID,
						message.DecryptedChecksums,
						e)
				}
				continue
			}

			// Copy the file and check is sizes match
			copiedSize, err := io.Copy(dest, file)
			if err != nil || copiedSize != int64(fileSize) {
				log.Errorf("Failed to copy file "+
					"(corr-id: %s, "+
					"filepath: %s, "+
					"user: %s, "+
					"accessionid: %s, "+
					"decryptedChecksums: %v, error: %v)",
					delivered.CorrelationId,
					message.Filepath,
					message.User,
					message.AccessionID,
					message.DecryptedChecksums,
					err)

				//FIXME: should it retry?
				if e := delivered.Nack(false, true); e != nil {
					log.Errorf("Failed to NAck because of Copy failed "+
						"(corr-id: %s, "+
						"filepath: %s, "+
						"user: %s, "+
						"accessionid: %s, "+
						"decryptedChecksums: %v, error: %v)",
						delivered.CorrelationId,
						message.Filepath,
						message.User,
						message.AccessionID,
						message.DecryptedChecksums,
						e)
				}
				continue
			}

			file.Close()
			dest.Close()

			log.Infof("Synced file "+
				"(corr-id: %s, "+
				"filepath: %s, "+
				"user: %s, "+
				"accessionid: %s, "+
				"decryptedChecksums: %v)",
				delivered.CorrelationId,
				message.Filepath,
				message.User,
				message.AccessionID,
				message.DecryptedChecksums)

			if err := mq.SendMessage(delivered.CorrelationId, conf.Broker.Exchange, conf.Broker.RoutingKey, conf.Broker.Durable, delivered.Body); err != nil {
				// TODO fix resend mechanism
				log.Errorf("Failed to send message for completed "+
					"(corr-id: %s, "+
					"filepath: %s, "+
					"user: %s, "+
					"accessionid: %s, "+
					"decryptedChecksums: %v, error: %v)",
					delivered.CorrelationId,
					message.Filepath,
					message.User,
					message.AccessionID,
					message.DecryptedChecksums,
					err)

				// Restart loop, do not ack
				continue
			}

			if err := delivered.Ack(false); err != nil {

				log.Errorf("Failed to ack message after work completed "+
					"(corr-id: %s, "+
					"filepath: %s, "+
					"user: %s, "+
					"accessionid: %s, "+
					"decryptedChecksums: %v, error: %v)",
					delivered.CorrelationId,
					message.Filepath,
					message.User,
					message.AccessionID,
					message.DecryptedChecksums,
					err)

			}
		}
	}()

	<-forever
}
