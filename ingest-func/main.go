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
	"sync"
	"time"

	speech "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/storage"
	"github.com/asticode/go-astisub"
	"google.golang.org/api/iterator"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CharsPerLine limits how many characters approximately we accept as one line
// This is based on Netflix recommendation:
// https://partnerhelp.netflixstudios.com/hc/en-us/articles/215274938-What-is-the-maximum-number-of-characters-per-line-allowed-in-Timed-Text-assets-
const CharsPerLine = 42

// DefaultFPS is used when no FPS info is provided.
const DefaultFPS = 25

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
	FPS             int32  `json:"fps"`
}

// FileStatus is the structure written into the storage to keep track of the status
type FileStatus struct {
	IngestRequest
	JobID      string `json:"job_id"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	SourceFile string `json:"source"`
	TxtFile    string `json:"txt_file"`
	JSONFile   string `json:"json_file"`
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

func durationToFrameNumber(d time.Duration, fps int32) int64 {
	// Protect against div by 0
	if fps == 0 || fps > 1001 {
		log.Printf("Warning: FPS must be between 1 and 1000. Falling back to %d. Provided: %d", DefaultFPS, fps)
		fps = DefaultFPS
	}

	milisPerFrame := 1000 / int64(fps)
	return d.Milliseconds() / milisPerFrame
}

func fmtDuration(d time.Duration, fps int32) string {
	return fmt.Sprintf("%02.f:%02d:%02d:%02d", d.Hours(), int64(d.Minutes())%60, int64(d.Seconds())%60, durationToFrameNumber(d, fps)%int64(fps))
}

func stringToSubItem(text string, start, end time.Duration) *astisub.Item {
	return &astisub.Item{
		StartAt: start,
		EndAt:   end,
		Lines: []astisub.Line{
			{
				Items: []astisub.LineItem{
					{
						Text: strings.TrimSpace(text),
					},
				},
			},
		},
	}

}

func transcriptionToSrt(trans []*speechpb.SpeechRecognitionResult) *astisub.Subtitles {
	subs := astisub.NewSubtitles()

	if len(trans) == 0 {
		return subs
	}

	line := ""
	firstWord := trans[0].Alternatives[0].Words[0]
	var lastWord *speechpb.WordInfo

	for _, r := range trans {
		alt := r.Alternatives[0]
		for _, w := range alt.Words {
			if len(line) > CharsPerLine {
				subs.Items = append(subs.Items, stringToSubItem(line, firstWord.StartTime.AsDuration(), lastWord.GetEndTime().AsDuration()))

				// Start a new line
				line = ""
				firstWord = w
			}

			line += " " + w.Word
			lastWord = w
		}
	}

	subs.Items = append(subs.Items, stringToSubItem(line, firstWord.StartTime.AsDuration(), lastWord.GetEndTime().AsDuration()))
	return subs
}

func transcriptionToPlainText(trans []*speechpb.SpeechRecognitionResult, fps int32, timestamps bool) string {
	if len(trans) == 0 {
		return ""
	}

	lines := ""
	line := ""

	charsPerLine := CharsPerLine
	if timestamps {
		// Compensate for the timestamp length
		charsPerLine += len(fmtDuration(1, fps))

		// Inject timestamp of the 1st word for the 1st line
		line = fmt.Sprintf("%s:", fmtDuration(trans[0].Alternatives[0].Words[0].StartTime.AsDuration(), fps))
	}

	for _, r := range trans {
		alt := r.Alternatives[0]
		for _, w := range alt.Words {
			if len(line) > CharsPerLine {
				lines += strings.TrimSpace(line) + "\n"

				// Start a new line
				if timestamps {
					line = fmt.Sprintf("%s:", fmtDuration(w.StartTime.AsDuration(), fps))
				} else {
					line = ""
				}
			}

			line += " " + w.Word
		}
	}

	// Append the last generated line if it was not empty
	if line != "" {
		lines += strings.TrimSpace(line) + "\n"
	}
	return lines
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

	var wg sync.WaitGroup
	for {
		attrs, err := objs.Next()
		if err == iterator.Done {
			break
		}

		if err != nil {
			log.Printf("Can't read file: %+v", err)
			continue
		}

		wg.Add(1)
		go resultWorker(ctx, &wg, client, ingestBucket, resultBucket, attrs)
	}

}

func resultWorker(ctx context.Context, wg *sync.WaitGroup, client *speech.Client, ingestBucket, resultBucket *storage.BucketHandle, attrs *storage.ObjectAttrs) {
	log.Printf("Processing: %s", attrs.Name)
	defer wg.Done()

	if !strings.HasSuffix(attrs.Name, ".json") {
		// Ignore non json files
		return
	}
	/*
		if err != nil {
			sendError(w, fmt.Sprintf("Bucket(%s).Objects(): %v", ingestBucketID, err), http.StatusInternalServerError)
			return
		}
	*/

	statusFile := ingestBucket.Object(attrs.Name)
	reader, err := statusFile.NewReader(ctx)
	if err != nil {
		log.Printf("Can't open status file: %+v", err)
		return
	}

	statusFileBytes, err := ioutil.ReadAll(reader)
	if err != nil {
		log.Printf("Can't read status file: %+v", err)
		return
	}

	fileStatus := FileStatus{}
	err = json.Unmarshal(statusFileBytes, &fileStatus)
	if err != nil {
		log.Printf("Can't decode json: %+v", err)
		return
	}

	if fileStatus.JobID == "" || fileStatus.Status != StatusProcessing {
		// Not sent to transcription yet or already handled. Take it next time
		return
	}

	op := client.LongRunningRecognizeOperation(fileStatus.JobID)
	resp, err := op.Poll(ctx)
	if err != nil {
		log.Printf("Can't get op status: %+v", err)
		fileStatus.Status = StatusError
		fileStatus.Error = err.Error()
		writeStatus(ctx, statusFile, fileStatus)
		return
	}

	if !op.Done() {
		log.Printf("%s not done yet", fileStatus.JobID)
		return
	}

	results := []*speechpb.SpeechRecognitionResult{}
	for _, r := range resp.GetResults() {
		results = append(results, r)
	}

	txtFile := resultBucket.Object(fmt.Sprintf("%s.txt", fileStatus.SourceFile))
	writer := txtFile.NewWriter(ctx)
	_, err = writer.Write([]byte(transcriptionToPlainText(results, fileStatus.FPS, true)))
	if err != nil {
		log.Printf("Error writing results: %+v", err)
		fileStatus.Status = StatusError
		fileStatus.Error = err.Error()
		writeStatus(ctx, statusFile, fileStatus)
		return
	}

	err = writer.Close()
	if err != nil {
		log.Printf("Error closing writer: %+v", err)
		fileStatus.Status = StatusError
		fileStatus.Error = err.Error()
		writeStatus(ctx, statusFile, fileStatus)
		return
	}

	srtFile := resultBucket.Object(fmt.Sprintf("%s.srt", fileStatus.SourceFile))
	writer = srtFile.NewWriter(ctx)
	subs := transcriptionToSrt(results)
	err = subs.WriteToSRT(writer)
	if err != nil {
		log.Printf("Error writing SRT: %+v", err)
		fileStatus.Status = StatusError
		fileStatus.Error = err.Error()
		writeStatus(ctx, statusFile, fileStatus)
		return
	}

	err = writer.Close()
	if err != nil {
		log.Printf("Error closing SRT: %+v", err)
		fileStatus.Status = StatusError
		fileStatus.Error = err.Error()
		writeStatus(ctx, statusFile, fileStatus)
		return
	}

	vttFile := resultBucket.Object(fmt.Sprintf("%s.vtt", fileStatus.SourceFile))
	writer = vttFile.NewWriter(ctx)
	err = subs.WriteToWebVTT(writer)
	if err != nil {
		log.Printf("Error writing VTT: %+v", err)
		fileStatus.Status = StatusError
		fileStatus.Error = err.Error()
		writeStatus(ctx, statusFile, fileStatus)
		return
	}

	err = writer.Close()
	if err != nil {
		log.Printf("Error closing VTT: %+v", err)
		fileStatus.Status = StatusError
		fileStatus.Error = err.Error()
		writeStatus(ctx, statusFile, fileStatus)
		return
	}

	fileStatus.Status = StatusCompleted
	fileStatus.TxtFile = txtFile.ObjectName()
	writeStatus(ctx, statusFile, fileStatus)
	ingestBucket.Object(fileStatus.SourceFile).Delete(ctx)
}

// Encoding as the protobuf version
func (r IngestRequest) Encoding() speechpb.RecognitionConfig_AudioEncoding {
	switch strings.ToUpper(r.EncodingString) {
	case "PCM":
		return speechpb.RecognitionConfig_LINEAR16
	case "OPUS":
		return speechpb.RecognitionConfig_OGG_OPUS
	case "FLAC":
		return speechpb.RecognitionConfig_FLAC
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

	if reqData.FPS == 0 {
		reqData.FPS = DefaultFPS
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
		SourceFile:    strings.TrimPrefix(fileURL.Path, "/"),
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
