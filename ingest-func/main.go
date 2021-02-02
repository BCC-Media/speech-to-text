package sst

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	speech "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/storage"
	"github.com/davecgh/go-spew/spew"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1"
)

// GCSEvent is the payload of a GCS event.
type GCSEvent struct {
	Kind                    string                 `json:"kind"`
	ID                      string                 `json:"id"`
	SelfLink                string                 `json:"selfLink"`
	Name                    string                 `json:"name"`
	Bucket                  string                 `json:"bucket"`
	Generation              string                 `json:"generation"`
	Metageneration          string                 `json:"metageneration"`
	ContentType             string                 `json:"contentType"`
	TimeCreated             time.Time              `json:"timeCreated"`
	Updated                 time.Time              `json:"updated"`
	TemporaryHold           bool                   `json:"temporaryHold"`
	EventBasedHold          bool                   `json:"eventBasedHold"`
	RetentionExpirationTime time.Time              `json:"retentionExpirationTime"`
	StorageClass            string                 `json:"storageClass"`
	TimeStorageClassUpdated time.Time              `json:"timeStorageClassUpdated"`
	Size                    string                 `json:"size"`
	MD5Hash                 string                 `json:"md5Hash"`
	MediaLink               string                 `json:"mediaLink"`
	ContentEncoding         string                 `json:"contentEncoding"`
	ContentDisposition      string                 `json:"contentDisposition"`
	CacheControl            string                 `json:"cacheControl"`
	Metadata                map[string]interface{} `json:"metadata"`
	CRC32C                  string                 `json:"crc32c"`
	ComponentCount          int                    `json:"componentCount"`
	Etag                    string                 `json:"etag"`
	CustomerEncryption      struct {
		EncryptionAlgorithm string `json:"encryptionAlgorithm"`
		KeySha256           string `json:"keySha256"`
	}
	KMSKeyName    string `json:"kmsKeyName"`
	ResourceState string `json:"resourceState"`
}

type CheckRequest struct {
	RequestID  string `json:"id"`
	OutputName string `json:"output"`
}

func CheckSTT(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	client, err := speech.NewClient(ctx)
	if err != nil {
		log.Fatal(err)
	}

	p := CheckRequest{}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		log.Printf("json.NewDecoder: %v", err)
		http.Error(w, "Error parsing request", http.StatusBadRequest)
		return
	}

	op := client.LongRunningRecognizeOperation(p.RequestID)
	resp, err := op.Poll(ctx)
	if err != nil {
		msg := fmt.Sprintf("Error getting resulst: %v", err)
		fmt.Println(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	if !op.Done() {
		msg := fmt.Sprintf("Operation %s is not done yet", op.Name())
		log.Println(msg)
		w.Write([]byte(msg))
		return
	}

	resp, err = op.Wait(r.Context())
	if err != nil {
		log.Fatal(err)
	}

	transcript := ""
	for _, r := range resp.GetResults() {
		transcript += r.Alternatives[0].Transcript
	}

	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		msg := fmt.Sprintf("Err %v ", err)
		log.Println(msg)
		w.Write([]byte(msg))
		return
	}

	bkt := storageClient.Bucket("transcribe-bcc-2-result")
	file := bkt.Object(fmt.Sprintf("%s.txt", op.Name()))
	writer := file.NewWriter(ctx)
	writer.Write([]byte(transcript))
	writer.Close()
	w.Write([]byte(transcript))

	/*
		opLR := client.LROClient.ListOperations(ctx, &longrunning.ListOperationsRequest{})

		i := 0
		for {
			fmt.Printf("%d", i)
			i++

			op, done := opLR.Next()
			if done == iterator.Done {
				break
			}

			opR := client.LongRunningRecognizeOperation(op.Name)
			if !opR.Done() {
				log.Println("Not done")
				meta, err := opR.Metadata()
				if err != nil {
					continue
				}

				spew.Dump(meta)
				continue
			}

			spew.Dump(opR.Wait(ctx))
		}
	*/
}

func SendSTT(ctx context.Context, e GCSEvent) error {
	spew.Dump(e)
	client, err := speech.NewClient(ctx)
	// Send the contents of the audio file with the encoding and
	// and sample rate information to be transcripted.
	req := &speechpb.LongRunningRecognizeRequest{
		Config: &speechpb.RecognitionConfig{
			Encoding:                   speechpb.RecognitionConfig_LINEAR16,
			SampleRateHertz:            48000,
			AudioChannelCount:          2,
			LanguageCode:               "no-NO",
			SpeechContexts:             []*speechpb.SpeechContext{},
			EnableAutomaticPunctuation: true,
			EnableWordTimeOffsets:      true,
		},
		Audio: &speechpb.RecognitionAudio{
			AudioSource: &speechpb.RecognitionAudio_Uri{Uri: fmt.Sprintf("gs://%s/%s", e.Bucket, e.Name)},
		},
	}

	op, err := client.LongRunningRecognize(ctx, req)
	if err != nil {
		return err
	}

	spew.Print(op.Name())
	return nil
}
