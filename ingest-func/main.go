package sst

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	speech "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/storage"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	apiKey = os.Getenv("FUNCTION_KEY")
)

func sendError(w http.ResponseWriter, message string, status int) {
	http.Error(w, message, status)
	log.Print(message)
}

func writeStatus(ctx context.Context, statusFile *storage.ObjectHandle, fStatus FileStatus) error {
	writer := statusFile.NewWriter(ctx)
	err := json.NewEncoder(writer).Encode(fStatus)
	if err != nil {
		return err
	}

	return writer.Close()
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
}

// IngestRequest captures the submitted data
type IngestRequest struct {
	File            string `json:"file"`
	Language        string `json:"lang"`
	EncodingString  string `json:"encoding"`
	SampleRateHertz int32  `json:"sample_rate"`
}

// FileStatus is the structure written into the storage to keep track of the status
type FileStatus struct {
	IngestRequest
	JobID string `json:"job_id"`
}

// Encoding as the protobuf version
func (r IngestRequest) Encoding() speechpb.RecognitionConfig_AudioEncoding {
	switch strings.ToUpper(r.EncodingString) {
	case "PCM":
		return speechpb.RecognitionConfig_LINEAR16
	case "OPUS":
		return speechpb.RecognitionConfig_OGG_OPUS
	}

	log.Printf("Unknown encoding: %s", r.EncodingString)
	return speechpb.RecognitionConfig_ENCODING_UNSPECIFIED
}

// Ingest starts the transcription process
func Ingest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.URL.Query().Get("key") != apiKey {
		sendError(w, "Wrong key", http.StatusUnauthorized)
		return
	}

	client, err := speech.NewClient(ctx)
	if err != nil {
		sendError(w, "Can't connect to speech API. See log for more details.", http.StatusBadRequest)
		return
	}

	reqData := IngestRequest{}
	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		sendError(w, "Error parsing request", http.StatusBadRequest)
		return
	}

	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		sendError(w, fmt.Sprintf("Unable to create a storage client: %+v", err), http.StatusInternalServerError)
		return
	}

	fileURL, err := url.Parse(reqData.File)
	if err != nil {
		sendError(w, fmt.Sprintf("Unable to parse file url: %+v", err), http.StatusInternalServerError)
		return
	}

	bucket := storageClient.Bucket(fileURL.Hostname())
	// The trim is required because in case of "/test.json" storage creates a folder called `/`
	statusFile := bucket.Object(strings.TrimPrefix(fmt.Sprintf("%s.json", fileURL.Path), "/"))

	_, err = statusFile.Attrs(ctx)
	if err != storage.ErrObjectNotExist {
		sendError(w, fmt.Sprintf("File is already in progress: %+v", err), http.StatusConflict)
		return
	}

	fStatus := FileStatus{
		IngestRequest: reqData,
	}

	err = writeStatus(ctx, statusFile, fStatus)
	if err != nil {
		sendError(w, fmt.Sprintf("Unable to write status file: %+v", err), http.StatusConflict)
		return
	}

	// Send the contents of the audio file with the encoding and
	// and sample rate information to be transcripted.
	req := &speechpb.LongRunningRecognizeRequest{
		Config: &speechpb.RecognitionConfig{
			Encoding:                   reqData.Encoding(),
			SampleRateHertz:            reqData.SampleRateHertz,
			AudioChannelCount:          2,
			LanguageCode:               reqData.Language,
			SpeechContexts:             []*speechpb.SpeechContext{},
			EnableAutomaticPunctuation: true,
			EnableWordTimeOffsets:      true,
		},
		Audio: &speechpb.RecognitionAudio{
			AudioSource: &speechpb.RecognitionAudio_Uri{Uri: reqData.File},
		},
	}

	op, err := client.LongRunningRecognize(ctx, req)
	if err != nil {
		errStatus, ok := status.FromError(err)
		if !ok {
			sendError(w, "Error starting job - Unknow error", http.StatusInternalServerError)
		} else if errStatus.Code() == codes.NotFound {
			sendError(w, fmt.Sprintf("Could not locate file \"%s\"", reqData.File), http.StatusNotFound)
		} else if errStatus.Code() == codes.InvalidArgument {
			sendError(w, fmt.Sprintf("Illegal argumet: \"%s\"", errStatus.Message()), http.StatusBadRequest)
		} else {
			sendError(w, "Error starting job", http.StatusInternalServerError)
		}

		_ = statusFile.Delete(ctx)
		return
	}

	fStatus.JobID = op.Name()

	err = writeStatus(ctx, statusFile, fStatus)
	if err != nil {
		sendError(w, fmt.Sprintf("Unable to write status file: %+v", err), http.StatusConflict)
		return
	}
	log.Printf("Op id: %s", op.Name())
}
