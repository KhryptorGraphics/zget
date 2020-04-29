package main

//go:generate git tag -af v$VERSION -m "v$VERSION"
//go:generate go run .github/updateversion.go
//go:generate git commit -am "bump $VERSION"
//go:generate git tag -af v$VERSION -m "v$VERSION"

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/schollz/httppool"
	log "github.com/schollz/logger"
	"github.com/schollz/progressbar/v3"
	"github.com/schollz/zget/src/torrent"
)

var flagWorkers int
var flagCompressed, flagVerbose, flagNoClobber, flagUseTor, flagDoStat, flagVersion, flagGzip bool
var flagList, flagOutfile string
var flagHeaders arrayFlags
var Version = "1.0.0"

type arrayFlags []string

func (i *arrayFlags) String() string {
	return ""
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

var hpool *httppool.HTTPPool

func init() {
	flag.BoolVar(&flagCompressed, "compressed", false, "whether to request compressed resources")
	flag.BoolVar(&flagVerbose, "v", false, "verbose")
	flag.BoolVar(&flagVersion, "version", false, "print version")
	flag.BoolVar(&flagGzip, "gzip", false, "gzip downloads")
	flag.BoolVar(&flagNoClobber, "nc", false, "no clobber")
	flag.BoolVar(&flagUseTor, "tor", false, "use tor")
	flag.BoolVar(&flagDoStat, "stat", false, "stat")
	flag.StringVar(&flagList, "i", "", "list of urls")
	flag.StringVar(&flagOutfile, "o", "", "filename to write to")
	flag.IntVar(&flagWorkers, "w", 1, "number of workers")
	flag.Var(&flagHeaders, "H", "set headers")
}

func main() {
	flag.Parse()
	log.SetOutput(os.Stderr)
	err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s", err.Error())
		os.Exit(1)
	}
}

var httpHeaders map[string]string

func run() (err error) {
	if flagVersion {
		fmt.Printf("zget v%s\n", Version)
		return
	}
	if strings.HasPrefix(flag.Args()[0], "magnet") || strings.HasSuffix(flag.Args()[0], ".torrent") {
		return torrent.Download(flag.Args()[0])
	}
	if flagUseTor && runtime.GOOS == "windows" {
		err = fmt.Errorf("tor not supported on windows")
		return
	}
	httpHeaders = make(map[string]string)
	for _, header := range flagHeaders {
		foo := strings.SplitN(header, ":", 2)
		if len(foo) != 2 {
			continue
		}
		httpHeaders[strings.TrimSpace(foo[0])] = strings.TrimSpace(foo[1])
	}

	if flagDoStat {
		visit(parseURL(flag.Args()[0]))
		os.Exit(0)
	}
	hpool = httppool.New(
		httppool.OptionDebug(false),
		httppool.OptionNumClients(flagWorkers),
		httppool.OptionUseTor(flagUseTor),
		httppool.OptionHeaders(httpHeaders),
	)
	if flagVerbose {
		log.SetLevel("debug")
	}

	if flagList != "" {
		err = downloadfromfile(flagList)
	} else {
		err = download(flag.Args()[0], true)
	}

	return
}

func downloadfromfile(fname string) (err error) {
	numLines, err := countLinesInFile(fname)
	if err != nil {
		return
	}

	f, err := os.Open(fname)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	bar := progressbar.NewOptions(
		numLines,
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowIts(),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() { fmt.Println(" ") }),
		progressbar.OptionSetWidth(10),
	)
	for scanner.Scan() {
		bar.Add(1)
		u := strings.TrimSpace(scanner.Text())
		bar.Describe(u)
		err = download(u, false)
		if err != nil {
			return
		}
	}

	err = scanner.Err()
	return
}

func download(u string, justone bool) (err error) {
	uparsed := parseURL(u)
	u = uparsed.String()
	fpath := path.Join(uparsed.Host, uparsed.Path)
	if strings.HasSuffix(u, "/") {
		fpath = path.Join(fpath, "index.html")
	}
	log.Debugf("fpath: %s", fpath)
	if justone {
		_, filename := filepath.Split(fpath)
		fpath = filename
	}
	log.Debugf("fpath: %s", fpath)

	if flagOutfile != "" {
		fpath = flagOutfile
	} else {
		var stat os.FileInfo
		stat, err = os.Stat(fpath)
		if err == nil {
			if flagNoClobber {
				log.Debugf("already have %s", fpath)
				return
			} else if stat.IsDir() {
				err = fmt.Errorf("'%s' is directory: can't overwrite", fpath)
				return
			} else if !stat.IsDir() {
				for addNum := 1; addNum < 1000000; addNum++ {
					if _, errStat := os.Stat(fmt.Sprintf("%s.%d", fpath, addNum)); errStat != nil {
						fpath = fmt.Sprintf("%s.%d", fpath, addNum)
						break
					}
				}
			}
		}
	}
	if flagGzip {
		fpath += ".gz"
	}

	log.Debugf("saving to %s", fpath)
	resp, err := hpool.Get(u)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	foldername, _ := filepath.Split(fpath)
	log.Debugf("foldername: %s", foldername)
	os.MkdirAll(foldername, 0755)

	f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	var writers []io.Writer

	var bar *progressbar.ProgressBar
	if justone {
		bar = progressbar.NewOptions(
			int(resp.ContentLength),
			progressbar.OptionSetWriter(os.Stderr),
			progressbar.OptionShowBytes(true),
			progressbar.OptionSetDescription(fpath),
			progressbar.OptionOnCompletion(func() { fmt.Println(" ") }),
			progressbar.OptionSetWidth(10),
		)
		defer func() {
			bar.Finish()
		}()
		writers = append(writers, bar)
	}
	if flagGzip {
		buf := bufio.NewWriter(f)
		defer buf.Flush()
		gz := gzip.NewWriter(buf)
		defer gz.Close()
		writers = append(writers, gz)
	} else {
		writers = append(writers, f)
	}
	dest := io.MultiWriter(writers...)
	_, err = io.Copy(dest, resp.Body)
	return
}

func countLinesInFile(fname string) (int, error) {
	f, err := os.Open(fname)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	return lineCounter(f)
}

func lineCounter(r io.Reader) (int, error) {
	buf := make([]byte, 32*1024)
	count := 0
	lineSep := []byte{'\n'}

	for {
		c, err := r.Read(buf)
		count += bytes.Count(buf[:c], lineSep)

		switch {
		case err == io.EOF:
			return count, nil

		case err != nil:
			return count, err
		}
	}
}
