package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/danvergara/morphos/pkg/files"
)

const (
	uploadFileFormField = "uploadFile"
	lzcUserHomePath     = "/lzcapp/run/mnt/home/"
)

var (
	//go:embed all:templates
	templatesHTML embed.FS

	//go:embed all:static
	staticFiles embed.FS
	// Upload path.
	// It is a variable now, which means that can be
	// cofigurable through a environment variable.
	uploadPath string
)

func init() {
	uploadPath = os.Getenv("MORPHOS_UPLOAD_PATH")
	if uploadPath == "" {
		uploadPath = "/tmp"
	}
}

// statusError struct is the error representation
// at the HTTP layer.
type statusError struct {
	error
	status int
}

// Unwrap method returns the inner error.
func (e statusError) Unwrap() error { return e.error }

// HTTPStatus returns a HTTP status code.
func HTTPStatus(err error) int {
	if err == nil {
		return 0
	}

	var statusErr interface {
		error
		HTTPStatus() int
	}

	// Checks if err implements the statusErr interface.
	if errors.As(err, &statusErr) {
		return statusErr.HTTPStatus()
	}

	// Returns a default status code if none is provided.
	return http.StatusInternalServerError
}

// WithHTTPStatus returns an error with the original error and the status code.
func WithHTTPStatus(err error, status int) error {
	return statusError{
		error:  err,
		status: status,
	}
}

// toHandler is a wrapper for functions that have the following signature:
// func(http.ResponseWriter, *http.Request) error
// So, regular handlers can return an error that can be unwrapped.
// If an errors is received at the time to execute the original handler,
// renderError function is called.
func toHandler(f func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := f(w, r)
		if err != nil {
			renderError(w, err.Error(), HTTPStatus(err))
		}
	}
}

type ConvertedFile struct {
	Filename string
	FileType string
}

func openLzcFile(w http.ResponseWriter, r *http.Request) error {
	filePath := r.URL.Query().Get("filePath")

	if filePath == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return nil
	}
	filePath = lzcUserHomePath + filePath

	data, err := os.ReadFile(filePath)

	if err != nil {
		log.Printf("get file error: %v", err)

		http.Redirect(w, r, "/", http.StatusFound)

		return nil
	}

	tmpls := []string{
		"templates/base.tmpl",
		"templates/partials/htmx.tmpl",
		"templates/partials/style.tmpl",
		"templates/partials/nav.tmpl",
		"templates/partials/form-lzc.tmpl",
		"templates/partials/modal.tmpl",
		"templates/partials/js.tmpl",
	}

	detectedFileType := mimetype.Detect(data)

	fileType, subType, err := files.TypeAndSupType(detectedFileType.String())
	if err != nil {
		log.Printf("error occurred getting type and subtype from mimetype: %v", err)
		return WithHTTPStatus(err, http.StatusBadRequest)
	}

	fileFactory, err := files.BuildFactory(fileType, "")
	if err != nil {
		log.Printf("error occurred while getting a file factory: %v", err)
		return WithHTTPStatus(err, http.StatusBadRequest)
	}

	f, err := fileFactory.NewFile(subType)
	if err != nil {
		log.Printf("error occurred getting the file object: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	tmpl, err := template.ParseFS(templatesHTML, tmpls...)
	if err != nil {
		log.Printf("error occurred parsing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	fileData := map[string]string{
		"filename": filepath.Base(filePath),
		"filepath": filePath,
	}
	mergeData := map[string]interface{}{
		"fileData":   fileData,
		"targetList": f.SupportedFormats(),
	}

	if err = tmpl.ExecuteTemplate(w, "base", mergeData); err != nil {
		log.Printf("error occurred executing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return nil
}

func index(w http.ResponseWriter, _ *http.Request) error {
	tmpls := []string{
		"templates/base.tmpl",
		"templates/partials/htmx.tmpl",
		"templates/partials/style.tmpl",
		"templates/partials/nav.tmpl",
		"templates/partials/form.tmpl",
		"templates/partials/modal.tmpl",
		"templates/partials/js.tmpl",
	}

	tmpl, err := template.ParseFS(templatesHTML, tmpls...)
	if err != nil {
		log.Printf("error ocurred parsing templates: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	err = tmpl.ExecuteTemplate(w, "base", nil)
	if err != nil {
		log.Printf("error ocurred executing template: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return nil
}

func handleUploadLzcFile(w http.ResponseWriter, r *http.Request) error {

	filePath := r.FormValue("filePath")
	fileName := r.FormValue("fileName")
	targetFormat := r.FormValue("targetFormat")

	byteData, err := os.ReadFile(filePath)

	if err != nil {
		return nil
	}

	convertedFileName, convertedFileType, _, err := convertFileLzc(byteData, targetFormat, fileName)
	if err != nil {
		return err
	}

	tmpls := []string{
		"templates/partials/card_file.tmpl",
		"templates/partials/modal.tmpl",
	}

	tmpl, err := template.ParseFS(templatesHTML, tmpls...)
	if err != nil {
		log.Printf("error occurred parsing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	err = tmpl.ExecuteTemplate(
		w,
		"content",
		ConvertedFile{Filename: convertedFileName, FileType: convertedFileType},
	)
	if err != nil {
		log.Printf("error occurred executing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return nil
}

func handleUploadFile(w http.ResponseWriter, r *http.Request) error {
	convertedFileName, convertedFileType, _, err := convertFile(r)
	if err != nil {
		return err
	}

	tmpls := []string{
		"templates/partials/card_file.tmpl",
		"templates/partials/modal.tmpl",
	}

	tmpl, err := template.ParseFS(templatesHTML, tmpls...)
	if err != nil {
		log.Printf("error occurred parsing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	err = tmpl.ExecuteTemplate(
		w,
		"content",
		ConvertedFile{Filename: convertedFileName, FileType: convertedFileType},
	)
	if err != nil {
		log.Printf("error occurred executing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return nil
}

func handleFileFormat(w http.ResponseWriter, r *http.Request) error {
	file, _, err := r.FormFile(uploadFileFormField)
	if err != nil {
		log.Printf("error ocurred while getting file from form: %v", err)
		return WithHTTPStatus(err, http.StatusBadRequest)
	}

	fileBytes, err := io.ReadAll(file)
	if err != nil {
		log.Printf("error occurred while executing template files: %v", err)
		return WithHTTPStatus(err, http.StatusBadRequest)
	}

	detectedFileType := mimetype.Detect(fileBytes)

	templates := []string{
		"templates/partials/form.tmpl",
	}

	fileType, subType, err := files.TypeAndSupType(detectedFileType.String())
	if err != nil {
		log.Printf("error occurred getting type and subtype from mimetype: %v", err)
		return WithHTTPStatus(err, http.StatusBadRequest)
	}

	fileFactory, err := files.BuildFactory(fileType, "")
	if err != nil {
		log.Printf("error occurred while getting a file factory: %v", err)
		return WithHTTPStatus(err, http.StatusBadRequest)
	}

	f, err := fileFactory.NewFile(subType)
	if err != nil {
		log.Printf("error occurred getting the file object: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	tmpl, err := template.ParseFS(templatesHTML, templates...)
	if err != nil {
		log.Printf("error occurred parsing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	if err = tmpl.ExecuteTemplate(w, "format-elements", f.SupportedFormats()); err != nil {
		log.Printf("error occurred executing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return nil
}

func handleModal(w http.ResponseWriter, r *http.Request) error {
	filename := r.URL.Query().Get("filename")
	filetype := r.URL.Query().Get("filetype")

	tmpls := []string{
		"templates/partials/active_modal.tmpl",
	}

	tmpl, err := template.ParseFS(templatesHTML, tmpls...)
	if err != nil {
		log.Printf("error occurred parsing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	if err = tmpl.ExecuteTemplate(w, "content", ConvertedFile{Filename: filename, FileType: filetype}); err != nil {
		log.Printf("error occurred executing template files: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return nil
}

func getFormats(w http.ResponseWriter, r *http.Request) error {
	resp, err := json.Marshal(supportedFormatsJSONResponse())
	if err != nil {
		log.Printf("error ocurred marshalling the response: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(resp); err != nil {
		log.Printf("error ocurred writting to the ResponseWriter : %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return nil
}

func uploadFile(w http.ResponseWriter, r *http.Request) error {
	_, _, convertedFileBytes, err := convertFile(r)
	if err != nil {
		return err
	}

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := w.Write(convertedFileBytes); err != nil {
		log.Printf("error occurred writing converted file to response writer: %v", err)
		return WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return nil
}

func newRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	fsUpload := http.FileServer(http.Dir(uploadPath))

	var staticFS = http.FS(staticFiles)
	fs := http.FileServer(staticFS)

	addRoutes(r, fs, fsUpload)

	return r
}

func apiRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/formats", toHandler(getFormats))
	r.Post("/upload", toHandler(uploadFile))
	return r
}

func addRoutes(r *chi.Mux, fs, fsUpload http.Handler) {
	r.HandleFunc("/healthz", healthz)
	r.Handle("/static/*", fs)
	r.Handle("/files/*", http.StripPrefix("/files", fsUpload))
	r.Get("/", toHandler(index))
	r.Get("/open", toHandler(openLzcFile))
	r.Post("/upload", toHandler(handleUploadFile))
	r.Post("/uploadLzc", toHandler(handleUploadLzcFile))
	r.Post("/format", toHandler(handleFileFormat))
	r.Get("/modal", toHandler(handleModal))

	// Mount the api router.
	r.Mount("/api/v1", apiRouter())
}

func run(ctx context.Context) error {
	port := os.Getenv("MORPHOS_PORT")

	// default port.
	if port == "" {
		port = "8080"
	}

	ctx, stop := signal.NotifyContext(ctx,
		os.Interrupt,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	defer stop()

	r := newRouter()

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", port),
		Handler: r,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "error listening and serving: %s\n", err)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		<-ctx.Done()

		log.Println("shutdown signal received")

		ctxTimeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		srv.SetKeepAlivesEnabled(false)

		if err := srv.Shutdown(ctxTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "error shutting down http server: %s\n", err)
		}

		log.Println("shutdown completed")
	}()

	wg.Wait()

	return nil
}

func main() {
	ctx := context.Background()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}

	log.Println("exiting...")
}

// renderError functions executes the error template.
func renderError(w http.ResponseWriter, message string, statusCode int) {
	w.WriteHeader(statusCode)
	tmpl, _ := template.ParseFS(templatesHTML, "templates/partials/error.tmpl")
	_ = tmpl.ExecuteTemplate(w, "error", struct{ ErrorMessage string }{ErrorMessage: message})
}

func fileNameWithoutExtension(fileName string) string {
	return strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
}

func filename(filename, extension string) string {
	return fmt.Sprintf("%s.%s", fileNameWithoutExtension(filename), extension)
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// convertFile handles everything required to convert a file.
// It returns the name of the file, the target file type, the file as a slice of bytes and a possible error.
// It is both used by the HTML form and the API.
func convertFile(r *http.Request) (string, string, []byte, error) {
	var (
		convertedFile     io.Reader
		convertedFilePath string
		convertedFileName string
		err               error
	)

	// Parse and validate file and post parameters.
	file, fileHeader, err := r.FormFile(uploadFileFormField)
	if err != nil {
		log.Printf("error ocurred getting file from form: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusBadRequest)
	}
	defer file.Close()

	// Get the content of the file in form of a slice of bytes.
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		log.Printf("error ocurred reading file: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusBadRequest)
	}

	// Get the sub-type of the input file from the form.
	targetFileSubType := r.FormValue("targetFormat")

	// Call Detect fuction to get the mimetype of the input file.
	detectedFileType := mimetype.Detect(fileBytes)

	// Parse the mimetype to get the type and the sub-type of the input file.
	fileType, subType, err := files.TypeAndSupType(detectedFileType.String())
	if err != nil {
		log.Printf("error occurred getting type and subtype from mimetype: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusBadRequest)
	}

	// Get the right factory based off the input file type.
	fileFactory, err := files.BuildFactory(fileType, fileHeader.Filename)
	if err != nil {
		log.Printf("error occurred while getting a file factory: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusBadRequest)
	}

	// Returns an object that implements the File interface based on the sub-type of the input file.
	f, err := fileFactory.NewFile(subType)
	if err != nil {
		log.Printf("error occurred getting the file object: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusBadRequest)
	}

	// Return the kind of the output file.
	targetFileType := files.SupportedFileTypes()[targetFileSubType]

	// Convert the file to the target format.
	// convertedFile is an io.Reader.
	convertedFile, err = f.ConvertTo(
		cases.Title(language.English).String(targetFileType),
		targetFileSubType,
		bytes.NewReader(fileBytes),
	)
	if err != nil {
		log.Printf("error ocurred while processing the input file: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}

	switch fileType {
	case "application", "text":
		targetFileSubType = "zip"
	}

	convertedFileName = filename(fileHeader.Filename, targetFileSubType)
	convertedFilePath = filepath.Join(uploadPath, convertedFileName)

	newFile, err := os.Create(convertedFilePath)
	if err != nil {
		log.Printf("error occurred while creating the output file: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}
	defer newFile.Close()

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(convertedFile); err != nil {
		log.Printf("error occurred while readinf from the converted file: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}

	convertedFileBytes := buf.Bytes()
	if _, err := newFile.Write(convertedFileBytes); err != nil {
		log.Printf("error occurred writing converted output to a file in disk: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}

	convertedFileMimeType := mimetype.Detect(convertedFileBytes)

	convertedFileType, _, err := files.TypeAndSupType(convertedFileMimeType.String())
	if err != nil {
		log.Printf("error occurred getting the file type of the result file: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return convertedFileName, convertedFileType, convertedFileBytes, nil
}

func convertFileLzc(file []byte, targetFileSubType string, originFileName string) (string, string, []byte, error) {
	var (
		convertedFile     io.Reader
		convertedFilePath string
		convertedFileName string
		err               error
	)

	// Call Detect fuction to get the mimetype of the input file.
	detectedFileType := mimetype.Detect(file)

	// Parse the mimetype to get the type and the sub-type of the input file.
	fileType, subType, err := files.TypeAndSupType(detectedFileType.String())
	if err != nil {
		log.Printf("error occurred getting type and subtype from mimetype: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusBadRequest)
	}

	// Get the right factory based off the input file type.
	fileFactory, err := files.BuildFactory(fileType, originFileName)
	if err != nil {
		log.Printf("error occurred while getting a file factory: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusBadRequest)
	}

	// Returns an object that implements the File interface based on the sub-type of the input file.
	f, err := fileFactory.NewFile(subType)
	if err != nil {
		log.Printf("error occurred getting the file object: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusBadRequest)
	}

	// Return the kind of the output file.
	targetFileType := files.SupportedFileTypes()[targetFileSubType]

	// Convert the file to the target format.
	// convertedFile is an io.Reader.
	convertedFile, err = f.ConvertTo(
		cases.Title(language.English).String(targetFileType),
		targetFileSubType,
		bytes.NewReader(file),
	)
	if err != nil {
		log.Printf("error ocurred while processing the input file: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}

	switch fileType {
	case "application", "text":
		targetFileSubType = "zip"
	}

	convertedFileName = filename(originFileName, targetFileSubType)
	convertedFilePath = filepath.Join(uploadPath, convertedFileName)

	newFile, err := os.Create(convertedFilePath)
	if err != nil {
		log.Printf("error occurred while creating the output file: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}
	defer newFile.Close()

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(convertedFile); err != nil {
		log.Printf("error occurred while readinf from the converted file: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}

	convertedFileBytes := buf.Bytes()
	if _, err := newFile.Write(convertedFileBytes); err != nil {
		log.Printf("error occurred writing converted output to a file in disk: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}

	convertedFileMimeType := mimetype.Detect(convertedFileBytes)

	convertedFileType, _, err := files.TypeAndSupType(convertedFileMimeType.String())
	if err != nil {
		log.Printf("error occurred getting the file type of the result file: %v", err)
		return "", "", nil, WithHTTPStatus(err, http.StatusInternalServerError)
	}

	return convertedFileName, convertedFileType, convertedFileBytes, nil
}

// supportedFormatsJSONResponse returns the supported formas as a map formatted to be shown as JSON.
// The intention of this is showing the supported formats to the client.
// Example:
// {"documents": ["docx", "xls"], "image": ["png", "jpeg"]}
func supportedFormatsJSONResponse() map[string][]string {
	result := make(map[string][]string)

	for k, v := range files.SupportedFileTypes() {
		result[v] = append(result[v], k)
	}

	return result
}
