package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 1 << 30
	http.MaxBytesReader(w, r.Body, maxMemory)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}
	videoData, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get video data", err)
		return
	}
	if videoData.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Wrong media type", err)
		return
	}
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()
	if _, err := io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to copy data", err)
		return
	}
	tempFile.Seek(0, io.SeekStart)

	key := make([]byte, 32)
	rand.Read(key)
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get aspect ratio", err)
		return
	}
	aspectPrefix := ""
	switch aspectRatio {
	case "16:9":
		aspectPrefix = "landscape/"
	case "9:16":
		aspectPrefix = "portrait/"
	case "other":
		aspectPrefix = "other/"
	}

	fsTempFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to create fast start file", err)
		return
	}
	fsTempFile, err := os.Open(fsTempFilePath)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to open fast start file", err)
		return
	}
	defer os.Remove(fsTempFilePath)
	defer fsTempFile.Close()

	idString := aspectPrefix + base64.RawURLEncoding.EncodeToString(key) + ".mp4"
	objectInput := s3.PutObjectInput{Bucket: &cfg.s3Bucket, Key: &idString, Body: fsTempFile, ContentType: &mediaType}
	if _, err := cfg.s3Client.PutObject(context.Background(), &objectInput); err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to upload object", err)
		return
	}
	newVideoURL := fmt.Sprintf("https://%s/%s", cfg.s3CfDistribution, idString) //https://<bucket-name>.s3.<region>.amazonaws.com/<key>
	videoData.VideoURL = &newVideoURL
	if err := cfg.db.UpdateVideo(videoData); err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to update video data", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoData)
}

func getVideoAspectRatio(filePath string) (string, error) {
	var b bytes.Buffer
	ffprobeCmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	ffprobeCmd.Stdout = &b

	if err := ffprobeCmd.Run(); err != nil {
		return "", err
	}
	type aspectRatio struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	var videoData struct {
		Streams []aspectRatio `json:"streams"`
	}
	if err := json.Unmarshal(b.Bytes(), &videoData); err != nil {
		return "", err
	}
	ratio := float64(videoData.Streams[0].Width) / float64(videoData.Streams[0].Height)
	if 0.56 < ratio && ratio < 0.57 {
		return "9:16", nil
	} else if 1.7 < ratio && ratio < 1.8 {
		return "16:9", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	fsFilePath := filePath + ".processing"
	ffmpegCmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", fsFilePath)

	if err := ffmpegCmd.Run(); err != nil {
		return "", err
	}
	return fsFilePath, nil
}
