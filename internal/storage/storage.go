package storage

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"

	log "github.com/sirupsen/logrus"
)

// Backend defines methods to be implemented by PosixBackend and S3Backend
type Backend interface {
	GetFileSize(filePath string) (int64, error)
	NewFileReader(filePath string) (io.ReadCloser, error)
	NewFileWriter(filePath string) (io.WriteCloser, error)
}

// PosixBackend encapsulates an io.Reader instance
type PosixBackend struct {
	FileReader io.Reader
	FileWriter io.Writer
	Location   string
	Size       int64
}

// PosixConf stores information about the POSIX storage backend
type PosixConf struct {
	Location string
}

// NewPosixBackend returns a PosixReader struct
func NewPosixBackend(c PosixConf) *PosixBackend {
	var reader io.Reader
	return &PosixBackend{FileReader: reader, Location: c.Location}
}

// NewFileReader returns an io.Reader instance
func (pb *PosixBackend) NewFileReader(filePath string) (io.ReadCloser, error) {
	file, err := os.Open(filepath.Join(filepath.Clean(pb.Location), filePath))
	if err != nil {
		log.Error(err)
		return nil, err
	}

	return file, nil
}

// NewFileWriter returns an io.Writer instance
func (pb *PosixBackend) NewFileWriter(filePath string) (io.WriteCloser, error) {
	file, err := os.OpenFile(filepath.Join(filepath.Clean(pb.Location), filePath), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0640)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	return file, nil
}

// GetFileSize returns the size of the file
func (pb *PosixBackend) GetFileSize(filePath string) (int64, error) {
	stat, err := os.Stat(filepath.Join(filepath.Clean(pb.Location), filePath))
	if err != nil {
		log.Error(err)
		return 0, err
	}

	return stat.Size(), nil
}

// S3Backend encapsulates a S3 client instance
type S3Backend struct {
	Client     *s3.S3
	Downloader *s3manager.Downloader
	Uploader   *s3manager.Uploader
	Bucket     string
}

// S3Conf stores information about the S3 storage backend
type S3Conf struct {
	URL               string
	Port              int
	AccessKey         string
	SecretKey         string
	Bucket            string
	Region            string
	UploadConcurrency int
	Chunksize         int
	Cacert            string
}

// NewS3Backend returns a S3Backend struct
func NewS3Backend(c S3Conf) *S3Backend {
	trConf := transportConfigS3(c)
	client := http.Client{Transport: trConf}
	session := session.Must(session.NewSession(
		&aws.Config{
			Endpoint:         aws.String(fmt.Sprintf("%s:%d", c.URL, c.Port)),
			Region:           aws.String(c.Region),
			HTTPClient:       &client,
			S3ForcePathStyle: aws.Bool(true),
			DisableSSL:       aws.Bool(strings.HasPrefix(c.URL, "http:")),
			Credentials:      credentials.NewStaticCredentials(c.AccessKey, c.SecretKey, ""),
		},
	))

	return &S3Backend{
		Bucket: c.Bucket,
		Uploader: s3manager.NewUploader(session, func(u *s3manager.Uploader) {
			u.PartSize = int64(c.Chunksize)
			u.Concurrency = c.UploadConcurrency
			u.LeavePartsOnError = false
		}),
		Downloader: s3manager.NewDownloader(session, func(d *s3manager.Downloader) {
			d.PartSize = int64(c.Chunksize)
			d.Concurrency = 1
		}),
		Client: s3.New(session)}
}

// NewFileReader returns an io.Reader instance
func (sb *S3Backend) NewFileReader(filePath string) (io.ReadCloser, error) {
	r, err := sb.Client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(sb.Bucket),
		Key:    aws.String(filePath),
	})

	if err != nil {
		log.Println(err)
	}

	return r.Body, nil
}

// NewFileWriter uploads the contents of an io.Reader to a S3 bucket
func (sb *S3Backend) NewFileWriter(filePath string) (io.WriteCloser, error) {
	reader, writer := io.Pipe()
	go func() {
		_, err := sb.Uploader.Upload(&s3manager.UploadInput{
			Body:            reader,
			Bucket:          aws.String(sb.Bucket),
			Key:             aws.String(filePath),
			ContentEncoding: aws.String("application/octet-stream"),
		})
		if err != nil {
			_ = reader.CloseWithError(err)
		}
	}()
	return writer, nil
}

// GetFileSize returns the size of a specific object
func (sb *S3Backend) GetFileSize(filePath string) (int64, error) {
	r, err := sb.Client.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(sb.Bucket),
		Key:    aws.String(filePath)})

	if err != nil {
		log.Println(err)
		return 0, err
	}

	return *r.ContentLength, nil
}

// transportConfigS3 is a helper method to setup TLS for the S3 client.
func transportConfigS3(c S3Conf) http.RoundTripper {
	cfg := new(tls.Config)

	// Enforce TLS1.2 or higher
	cfg.MinVersion = 2

	// Read system CAs
	var systemCAs, _ = x509.SystemCertPool()
	if reflect.DeepEqual(systemCAs, x509.NewCertPool()) {
		log.Debug("creating new CApool")
		systemCAs = x509.NewCertPool()
	}
	cfg.RootCAs = systemCAs

	if c.Cacert != "" {
		cacert, e := ioutil.ReadFile(c.Cacert) // #nosec this file comes from our configuration
		if e != nil {
			log.Fatalf("failed to append %q to RootCAs: %v", cacert, e)
		}
		if ok := cfg.RootCAs.AppendCertsFromPEM(cacert); !ok {
			log.Debug("no certs appended, using system certs only")
		}
	}

	var trConfig http.RoundTripper = &http.Transport{
		TLSClientConfig:   cfg,
		ForceAttemptHTTP2: true}

	return trConfig
}
