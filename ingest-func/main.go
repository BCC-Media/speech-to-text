package sst

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	speech "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/storage"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1"
)

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

type ingestRequest struct {
	File            string `json:"file"`
	Language        string `json:"lang"`
	EncodingString  string `json:"encoding"`
	SampleRateHertz int32  `json:"sample_rate"`
}

func (r ingestRequest) Encoding() speechpb.RecognitionConfig_AudioEncoding {
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

	client, err := speech.NewClient(ctx)
	if err != nil {
		http.Error(w, "Can't connect to speech API. See log for more details.", http.StatusBadRequest)
		log.Panicf("Can't connect to speech API: %+v", err)
		return
	}

	reqData := ingestRequest{}
	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		http.Error(w, "Error parsing request", http.StatusBadRequest)
		log.Panicf("json.NewDecoder: %v", err)
		return
	}

	// Send the contents of the audio file with the encoding and
	// and sample rate information to be transcripted.
	req := &speechpb.LongRunningRecognizeRequest{
		Config: &speechpb.RecognitionConfig{
			Encoding:                   speechpb.RecognitionConfig_LINEAR16,
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
		http.Error(w, "Error starting job", http.StatusInternalServerError)
		log.Panicf("json.NewDecoder: %v", err)
		return
	}

	log.Printf("Op id: %s", op.Name())
}
