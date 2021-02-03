package stt

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	speech "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	apiKey         = os.Getenv("FUNCTION_KEY")
	ingestBucketID = os.Getenv("INGEST_BUCKET")
	resultBucketID = os.Getenv("RESULT_BUCKET")
)

// Status constants
const (
	StatusProcessing = "processing"
	StatusError      = "error"
	StatusCompleted  = "completed"
)

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
	JobID    string `json:"job_id"`
	Status   string `json:"status"`
	Error    string `json:"error"`
	TxtFile  string `json:"txt_file"`
	JSONFile string `json:"json_file"`
}

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

// ProcessResults is called periodically to fetch teh finished transcriptions
// Should it become a longer process, we can inwoke it 1x per file via PubSub
func ProcessResults(w http.ResponseWriter, r *http.Request) {
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

	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		sendError(w, fmt.Sprintf("Unable to create a storage client: %+v", err), http.StatusInternalServerError)
		return
	}

	ingestBucket := storageClient.Bucket(ingestBucketID)
	resultBucket := storageClient.Bucket(resultBucketID)
	objs := ingestBucket.Objects(ctx, &storage.Query{Prefix: "status/"})
	i := 0
	for {
		attrs, err := objs.Next()

		if err == iterator.Done {
			log.Printf("Done: %d", i)
			break
		}

		i++
		log.Printf("Processing: %d", i)

		if !strings.HasSuffix(attrs.Name, ".json") {
			// Ignore non json files
			continue
		}

		if err != nil {
			sendError(w, fmt.Sprintf("Bucket(%s).Objects(): %v", ingestBucketID, err), http.StatusInternalServerError)
			return
		}

		log.Printf("Name: %s", attrs.Name)
		statusFile := ingestBucket.Object(attrs.Name)
		reader, err := statusFile.NewReader(ctx)
		if err != nil {
			sendError(w, fmt.Sprintf("Can't open status file: %+v", err), http.StatusInternalServerError)
			return
		}

		statusFileBytes, err := ioutil.ReadAll(reader)
		if err != nil {
			sendError(w, fmt.Sprintf("Can't read status file: %+v", err), http.StatusInternalServerError)
			return
		}

		fileStatus := FileStatus{}
		err = json.Unmarshal(statusFileBytes, &fileStatus)
		if err != nil {
			sendError(w, fmt.Sprintf("Can't decode json: %+v", err), http.StatusInternalServerError)
			return
		}

		if fileStatus.JobID == "" || fileStatus.Status != StatusProcessing {
			// Not sent to transcription yet or already handled. Take it next time
			continue
		}

		op := client.LongRunningRecognizeOperation(fileStatus.JobID)
		resp, err := op.Poll(ctx)
		if err != nil {
			sendError(w, fmt.Sprintf("Can't get op status: %+v", err), http.StatusInternalServerError)
			fileStatus.Status = StatusError
			fileStatus.Error = err.Error()
			writeStatus(ctx, statusFile, fileStatus)
			return
		}

		if !op.Done() {
			log.Printf("%s not done yet", fileStatus.JobID)
		}

		transcript := ""
		for _, r := range resp.GetResults() {
			transcript += r.Alternatives[0].Transcript
		}

		// TODO: errors
		file := resultBucket.Object(fmt.Sprintf("%s.txt", attrs.Name))
		writer := file.NewWriter(ctx)
		writer.Write([]byte(transcript))
		writer.Close()

		statusFile.Delete(ctx)
	}

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
	statusFile := bucket.Object(fmt.Sprintf("status%s.json", fileURL.Path))

	_, err = statusFile.Attrs(ctx)
	if err != storage.ErrObjectNotExist {
		sendError(w, fmt.Sprintf("File is already in progress: %+v", err), http.StatusConflict)
		return
	}

	fStatus := FileStatus{
		IngestRequest: reqData,
		Status:        StatusProcessing,
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

		errorText := fmt.Sprintf("Error starting job: %+v", err)
		httpCode := http.StatusInternalServerError

		if !ok {
			errorText = fmt.Sprintf("Error starting job - Unknown error: %+v", err)
		} else if errStatus.Code() == codes.NotFound {
			errorText = fmt.Sprintf("Could not locate file \"%s\"", reqData.File)
			httpCode = http.StatusNotFound
		} else if errStatus.Code() == codes.InvalidArgument {
			errorText = fmt.Sprintf("Illegal argumet: \"%s\"", errStatus.Message())
			httpCode = http.StatusBadRequest
		}

		sendError(w, errorText, httpCode)

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
