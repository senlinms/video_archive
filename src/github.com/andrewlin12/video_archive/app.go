package main

import (
  "bufio"
  "crypto/md5"
  "encoding/json"
  "flag"
  "fmt"
  "html/template"
  "io"
  "io/ioutil"
  "net/http"
  "os"
  "os/exec"
  "path"
  "strconv"
  "strings"
  "sync"
  "time"
  "github.com/gorilla/mux"
  "launchpad.net/goamz/aws"
  "launchpad.net/goamz/s3"
)

type JsonConfig struct {
  AccessKey string
  SecretKey string
  BucketName string
}

type VideoMetadata struct {
  OriginalFileName string
  Title string
  Description string
  Duration float64
  Status string
  DateTaken int64
  DateUploaded int64
}

var config JsonConfig
var s3Auth aws.Auth
var uploadMutex *sync.Mutex

func main() {
  uploadMutex = &sync.Mutex{}

  // Read config from disk
  configFile, e := ioutil.ReadFile("./config.json")
  if e != nil {
    fmt.Printf("Must have config.json\n")
    os.Exit(1)
  }
  json.Unmarshal(configFile, &config)
  fmt.Printf("AccessKey: %s\n", config.AccessKey)
  fmt.Printf("SecretKey: %s\n", config.SecretKey)
  fmt.Printf("BucketName: %s\n", config.BucketName)

  s3Auth = aws.Auth{
      AccessKey: config.AccessKey,
      SecretKey: config.SecretKey,
  }

  // Set up web routes
  router := mux.NewRouter()
  router.HandleFunc("/", index).Methods("GET")
  router.HandleFunc("/upload", handleUpload)
  router.HandleFunc("/video/{id}", video)
  router.HandleFunc("/videos", videos)

  // Static routes
  pubFileServer := http.FileServer(http.Dir("./pub/"))
  for _, path := range [...]string{"js", "css", "img", "tmpl"} {
    prefix := fmt.Sprintf("/%s/", path)
    router.PathPrefix(prefix).Handler(pubFileServer).Methods("GET")
  }

  http.Handle("/", router)

  var port = flag.Int("port", 3000, "Port to listen for requests");
  flag.Parse()

  fmt.Printf("Listening on %d...\n", *port);
  http.ListenAndServe(fmt.Sprintf(":%d", *port), nil);
}

func getS3Bucket() *s3.Bucket {
  // Connect to S3
  s3Connection := s3.New(s3Auth, aws.USEast)
  s3Bucket := s3Connection.Bucket(config.BucketName)
  return s3Bucket
}

var templates, _ = template.New("index").ParseFiles("./tmpl/index.html")
func index(w http.ResponseWriter, r *http.Request) {
  templates.ExecuteTemplate(w, "index.html", nil)
}

type VideosJson struct {
  Bucket string
  Ids []string
}
func videos(w http.ResponseWriter, r *http.Request) {
  s3Bucket := getS3Bucket()
  res, _ := s3Bucket.List("", "/", "", 1000)
  keys := make([]string, 0)
  for _, v := range res.CommonPrefixes {
    keys = append(keys, strings.Replace(v, "/", "", -1))
  }

  w.Header().Set("Content-Type", "application/json")
  json.NewEncoder(w).Encode(VideosJson{
    Bucket: config.BucketName,
    Ids: keys,
  })
}

func video(w http.ResponseWriter, r *http.Request) {
  vars := mux.Vars(r)
  s3Bucket := getS3Bucket()
  data, err := s3Bucket.Get(vars["id"] + "/metadata.json")
  if err != nil {
    http.Error(w, "Not Found", 404)
    return
  }
  w.Header().Set("Content-Type", "application/json")
  w.Write(data)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
  id := r.FormValue("resumableIdentifier")
  chunkNum, _ := strconv.ParseInt(r.FormValue("resumableChunkNumber"),
      10, 32)

  folderPath := fmt.Sprintf("/tmp/%s", id)
  filePath := fmt.Sprintf("%s/%08d", folderPath, chunkNum)
  if r.Method == "GET" {
    _, err := os.Stat(filePath)
    if err != nil {
      http.Error(w, "No Content", 204)
      return
    }

    fmt.Fprintf(w, "Found")
  } else if r.Method == "POST" {
    err := os.Mkdir(folderPath, 0744)
    file, _, err := r.FormFile("file")
    if err != nil {
      http.Error(w, "Could not read form data", 500)
      return
    }

    data, err := ioutil.ReadAll(file)
    if err != nil {
      fmt.Printf("Could not get form file: %v", err)
      http.Error(w, "Could not read file", 500)
      return
    }

    // Check if we are done
    expectedCount, _ := strconv.ParseInt(
        r.FormValue("resumableTotalChunks"), 10, 32)
    filename := r.FormValue("resumableFilename")

    // NOTE: Need to write the file and check if we are done in a mutex
    uploadMutex.Lock()
    err = ioutil.WriteFile(filePath, data, 0744)
    fileInfos, _ := ioutil.ReadDir(folderPath)
    uploadMutex.Unlock()

    if err != nil {
      fmt.Printf("Uh oh: %v", err);
      http.Error(w, "Could not create file", 500)
      return
    }


    if int(expectedCount) == len(fileInfos) {
      uploadComplete(w, folderPath, filename, fileInfos)
    }
    fmt.Fprintf(w, "Saved")
  } else {
    http.Error(w, "Unsupported", 505)
  }
}

func uploadComplete(w http.ResponseWriter, folderPath string, 
    filename string, fileInfos []os.FileInfo) {
  outputPath := fmt.Sprintf("/tmp/%s", filename)
  output, _ := os.Create(outputPath)
  for _, fileInfo := range fileInfos {
    chunkData, err := ioutil.ReadFile(
        fmt.Sprintf("%s/%s", folderPath, fileInfo.Name()))
    if err != nil {
      fmt.Printf("Error reading chunk: %v", err)
    }
    output.Write(chunkData)
  }
  fmt.Printf("Complete file: %s\n", outputPath)
  os.RemoveAll(folderPath)

  // ffprobe some metadata out
  cmd := exec.Command("ffprobe",
    "-show_streams",
    "-i", outputPath,
  )
  stdout, _ := cmd.StdoutPipe()
  scanner := bufio.NewScanner(stdout)
  err := cmd.Start()
  duration := 0.0
  dateTaken := time.Now().Unix() 
  for scanner.Scan() {
    scanned := scanner.Text()
    if strings.Index(scanned, "duration=") != -1 {
      parts := strings.SplitN(scanned, "=", 2)
      duration, err = strconv.ParseFloat(parts[1], 64)
    }
    if (strings.Index(scanned, "TAG:creation_time") != -1) {
      parts := strings.SplitN(scanned, "creation_time=", 2)
      parsedDate, err := time.Parse("2006-01-02 15:04:05", 
          strings.TrimSpace(parts[1]))
      if err == nil {
        dateTaken = parsedDate.Unix()
      }
    }
  }
  cmd.Wait()

  originalBaseName := path.Base(outputPath)
  md5Hash := md5.New()
  io.WriteString(md5Hash, fmt.Sprintf("%s|%d", originalBaseName, 
      time.Now().Unix()))
  basename := fmt.Sprintf("%d_%x", dateTaken, md5Hash.Sum([]byte{}))

  // Create a thumbnail
  thumbPath := "/tmp/" + basename + "_thumb.jpg"
  cmd = exec.Command("ffmpeg",
    "-i", outputPath,
    "-vframes", "1",
    thumbPath,
  )
  cmd.Run();
  if err != nil {
    fmt.Printf("Could not generate thumbnail: %v\n", err)
    return
  }
  fmt.Printf("Thumbnail complete: %s\n", thumbPath)
  uploadVideoFile(thumbPath, basename)

  metadata := VideoMetadata{
    Title: originalBaseName,
    OriginalFileName: originalBaseName,
    Description: fmt.Sprintf("Recorded %s", 
        time.Unix(dateTaken, 0).Format("Jan 2, 2006 3:04PM")),
    Duration: duration,
    DateTaken: dateTaken,
    DateUploaded: time.Now().Unix(),
    Status: "Processing",
  }
  jsonMetadata, _ := json.Marshal(metadata)

  _ = getS3Bucket().Put("/" + basename + "/metadata.json", 
    []byte(jsonMetadata), "text/json", s3.PublicRead)
  fmt.Printf("Metadata written\n")

  // NOTE: Do video transcodes in a goroutine
  go func() {
    video1080Path := "/tmp/" + basename + "_1080.mp4"
    video720Path := "/tmp/" + basename + "_720.mp4"
    video360Path := "/tmp/" + basename + "_360.mp4"
    cmd := exec.Command("ffmpeg", 
        "-i", outputPath,
        "-y",
        "-s", "1920x1080",
        "-vcodec", "libx264",
        "-acodec", "libfaac",
        video1080Path,

        "-y",
        "-s", "1280x720",
        "-vcodec", "libx264",
        "-acodec", "libfaac",
        video720Path,

        "-y",
        "-s", "640x360",
        "-vcodec", "libx264",
        "-acodec", "libfaac",
        video360Path,
    )
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    err := cmd.Run()
    if err != nil {
      fmt.Printf("Could not transcode file: %v\n", err)
      return
    }
    os.RemoveAll(outputPath)
    fmt.Printf("Transcode complete\n")

    for _, videoPath := range [...]string{ video360Path,
        video720Path, video1080Path } {
      uploadVideoFile(videoPath, basename)
    }

    metadata.Status = "Ready"
    jsonMetadata, _ := json.Marshal(metadata)
    _ = getS3Bucket().Put("/" + basename + "/metadata.json", 
        []byte(jsonMetadata), "text/json", s3.PublicRead)
    fmt.Printf("Final metadata written\n")
  }()
}

func uploadVideoFile(filePath string, basename string) {
  stat, _ := os.Stat(filePath)
  uploadFilename := strings.Replace(filePath, "/tmp", basename, -1)
  reader, _ := os.Open(filePath)
  var contentType string
  if (path.Ext(filePath) == ".jpg") {
    contentType = "image/jpg"
  } else { 
    contentType = "video/mp4"
  }
  err := getS3Bucket().PutReader(uploadFilename, reader, stat.Size(),
      contentType, s3.PublicRead)
  if err != nil {
    fmt.Printf("Failed to upload %s: %v\n", filePath, err)
  } else {
    fmt.Printf("Upload of %s complete\n", uploadFilename)
    os.RemoveAll(filePath)
  }
}
