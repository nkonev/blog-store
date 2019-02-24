package handlers

import (
	"context"
	"fmt"
	"github.com/labstack/echo/v4"
	"github.com/labstack/gommon/log"
	"github.com/minio/minio-go"
	"github.com/mongodb/mongo-go-driver/bson"
	"github.com/mongodb/mongo-go-driver/mongo"
	"github.com/mongodb/mongo-go-driver/mongo/options"
	"github.com/nkonev/blog-store/utils"
	"net/http"
	"net/url"
	"strconv"
)

func NewFsHandler(minio *minio.Client, serverUrl string, client *mongo.Client) *FsHandler {
	return &FsHandler{minio: minio, serverUrl: serverUrl, mongo: client}
}

type FsHandler struct {
	serverUrl string
	minio     *minio.Client
	mongo     *mongo.Client
}

type FileInfo struct {
	Filename string `json:"filename"`
	Url      string `json:"url"`
	Size     int64  `json:"size"`
}

func (h *FsHandler) LsHandler(c echo.Context) error {
	log.Debugf("Get userId: %v; userLogin: %v", c.Get(utils.USER_ID), c.Get(utils.USER_LOGIN))

	bucket := h.ensureAndGetBucket(c)
	// Create a done channel.
	doneCh := make(chan struct{})
	defer close(doneCh)

	log.Debugf("Listing bucket '%v':", bucket)

	var list []FileInfo = make([]FileInfo, 0)
	for objInfo := range h.minio.ListObjects(bucket, "", false, doneCh) {
		log.Debugf("Object '%v'", objInfo.Key)

		var downloadUrl *url.URL
		downloadUrl, err := url.Parse(h.serverUrl)
		if err != nil {
			return err
		}
		downloadUrl.Path += utils.DOWNLOAD_PREFIX + objInfo.Key

		list = append(list, FileInfo{Filename: objInfo.Key, Url: downloadUrl.String(), Size: objInfo.Size})
	}

	return c.JSON(http.StatusOK, &utils.H{"status": "ok", "files": list})
}

const FormFile = "file"

func (h *FsHandler) UploadHandler(c echo.Context) error {

	file, err := c.FormFile(FormFile)
	if err != nil {
		log.Errorf("Error during extracting form %v parameter", FormFile)
		return err
	}
	src, err := file.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	bucketName := h.ensureAndGetBucket(c)

	contentType := file.Header.Get("Content-Type")

	log.Debugf("Determined content type: %v", contentType)

	if _, err := h.minio.PutObject(bucketName, file.Filename, src, file.Size, minio.PutObjectOptions{ContentType: contentType}); err != nil {
		log.Errorf("Error during upload object: %v", err)
		return c.JSON(http.StatusInternalServerError, &utils.H{"status": "fail"})
	}

	return c.JSON(http.StatusOK, &utils.H{"status": "ok"})
}

func getBucketName(c echo.Context) string {
	i, ok := c.Get(utils.USER_ID).(int)
	if !ok {
		log.Errorf("Error during get(cast) userId")
	}
	return getBucketNameInt(i)
}

func getBucketNameInt(userId interface{}) string {
	return fmt.Sprintf(utils.USER_PREFIX+"%v", userId)
}


func getBucketLocation(c echo.Context) string {
	return "europe-east"
}

func (h *FsHandler) ensureAndGetBucket(c echo.Context) string {
	bucketName := getBucketName(c)
	bucketLocation := getBucketLocation(c)
	h.ensureBucket(bucketName, bucketLocation)
	return bucketName
}

func (h *FsHandler) ensureBucket(bucketName, location string) {
	err := h.minio.MakeBucket(bucketName, location)
	if err != nil {
		// Check to see if we already own this bucket (which happens if you run this twice)
		exists, err := h.minio.BucketExists(bucketName)
		if err == nil && exists {
			log.Debugf("Bucket '%s' already present", bucketName)
		} else {
			log.Fatal(err)
		}
	} else {
		log.Infof("Successfully created bucket '%s'", bucketName)
	}

}


func (h *FsHandler) download(bucketName, objName string) func(c echo.Context) error {
	return func(c echo.Context) error {
		info, e := h.minio.StatObject(bucketName, objName, minio.StatObjectOptions{})
		if e != nil {
			return c.JSON(http.StatusNotFound, &utils.H{"status": "stat fail"})
		}

		c.Response().Header().Set(echo.HeaderContentLength, strconv.FormatInt(info.Size, 10))
		c.Response().Header().Set(echo.HeaderContentType, info.ContentType)

		object, e := h.minio.GetObject(bucketName, objName, minio.GetObjectOptions{})
		defer object.Close()
		if e != nil {
			return c.JSON(http.StatusInternalServerError, &utils.H{"status": "fail"})
		}

		return c.Stream(http.StatusOK, info.ContentType, object)
	}
}


func (h *FsHandler) DownloadHandler(c echo.Context) error {
	bucketName := h.ensureAndGetBucket(c)

	objName := getFileName(c)

	return h.download(bucketName, objName)(c)
}

func getPublishDocument(objName string) bson.D {
	return bson.D{{"_id", objName}}
}

func (h *FsHandler) PublicDownloadHandler(c echo.Context) error {
	database := utils.GetMongoDatabase(h.mongo)

	bucketName := getBucketNameInt(c.Param("userId"))

	objName := getFileName(c)

	findResult := database.Collection(bucketName).FindOne(context.TODO(), getPublishDocument(objName))
	if findResult.Err() != nil {
		log.Errorf("Error during querying record from mongo")
		return findResult.Err()
	}
	err := findResult.Decode(nil)

	if err == mongo.ErrNoDocuments {
		return c.JSON(http.StatusNotFound, &utils.H{"status": "access fail"})
	}

	return h.download(bucketName, objName)(c)
}


func getFileName(context echo.Context) string {
	return context.Param("file")
}

func (h *FsHandler) MoveHandler(c echo.Context) error {
	from := c.Param("from")
	to := c.Param("to")
	bucketName := h.ensureAndGetBucket(c)

	info, e := h.minio.StatObject(bucketName, from, minio.StatObjectOptions{})
	if e != nil {
		return c.JSON(http.StatusNotFound, &utils.H{"status": "stat fail"})
	}

	object, err := h.minio.GetObject(bucketName, from, minio.GetObjectOptions{})
	defer object.Close()
	if err != nil {
		log.Errorf("Error during get object: %v", err)
		return c.JSON(http.StatusInternalServerError, &utils.H{"status": "fail"})
	}

	if _, err := h.minio.PutObject(bucketName, to, object, info.Size, minio.PutObjectOptions{ContentType: info.ContentType}); err != nil {
		log.Errorf("Error during copy object: %v", err)
		return c.JSON(http.StatusInternalServerError, &utils.H{"status": "fail"})
	}

	return c.JSON(http.StatusOK, &utils.H{"status": "ok"})
}

func (h *FsHandler) DeleteHandler(c echo.Context) error {
	bucketName := h.ensureAndGetBucket(c)
	objName := getFileName(c)
	objName, err := url.PathUnescape(objName)
	if err != nil {
		return err
	}
	if err := h.minio.RemoveObject(bucketName, objName); err != nil {
		log.Errorf("Error during remove object: %v", err)
		return c.JSON(http.StatusInternalServerError, &utils.H{"status": "fail"})
	}
	return c.JSON(http.StatusOK, &utils.H{"status": "ok"})
}

func (h *FsHandler) Limits(c echo.Context) error {
	bucketName := h.ensureAndGetBucket(c)

	var totalBucketConsumption int64

	recursive := true
	doneCh := make(chan struct{})
	defer close(doneCh)

	log.Debugf("Listing bucket '%v':", bucketName)
	for objInfo := range h.minio.ListObjects(bucketName, "", recursive, doneCh) {
		totalBucketConsumption += objInfo.Size
	}

	return c.JSON(http.StatusOK, &utils.H{"status": "ok", "used": totalBucketConsumption})
}

func (h *FsHandler) Publish(c echo.Context) error {
	bucketName := h.ensureAndGetBucket(c)

	objName := getFileName(c)
	objName, err := url.PathUnescape(objName)
	if err != nil {
		return err
	}
	_, e := h.minio.StatObject(bucketName, objName, minio.StatObjectOptions{})
	if e != nil {
		return c.JSON(http.StatusNotFound, &utils.H{"status": "stat fail"})
	}

	database := utils.GetMongoDatabase(h.mongo)

	upsert := true
	_, err2 := database.Collection(bucketName).UpdateOne(context.TODO(), getPublishDocument(objName), bson.D{}, &options.UpdateOptions{Upsert: &upsert})
	if err2 != nil {
		log.Errorf("Error during publishing '%v' : %v", objName, err)
		return err2
	}
	return c.JSON(http.StatusOK, &utils.H{"status": "ok", "published": true})
}