package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/goproxyio/goproxy/module"
)

var cacheDir string
var configFile string

type confStr struct {
	Listen    string
	MirrorMap map[string]string
	GoPath    string
}

var confData confStr

func init() {
	flag.StringVar(&configFile, "f", "/data/goproxy.conf", "config file")
	flag.Parse()
}

type JsonStruct struct {
}

func NewJsonStruct() *JsonStruct {
	return &JsonStruct{}
}

func (jst *JsonStruct) Load(filename string, v interface{}) error {
	//ReadFile函数会读取文件的全部内容，并将结果以[]byte类型返回
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		fmt.Printf("read file fail\n")
		return err
	}

	fmt.Fprintf(os.Stdout, "conf: %s\n", data)
	//读取的数据为json格式，需要进行解码
	err = json.Unmarshal(data, v)
	if err != nil {
		fmt.Printf("json uncode fail, error:%v\n", err)
		return err
	}
	fmt.Fprintf(os.Stdout, "%v\n", v)
	return nil
}

func main() {
	jStruct := NewJsonStruct()
	fmt.Printf("config file %s\n", configFile)
	err := jStruct.Load(configFile, &confData)
	if err != nil {
		panic("load config error")
	}
	fmt.Printf("confData: %v\n", confData)
	fmt.Printf("mirrorMap, %v\n", confData.MirrorMap)
	fmt.Printf("listen: %s\n", confData.Listen)
	fmt.Printf(": %s\n", confData.Listen)
	//需要设置一下GO111MODULE
	os.Setenv("GO111MODULE", "on")

	var gopath string
	if confData.GoPath == "" {
		gpEnv := os.Getenv("GOPATH")
		if gpEnv == "" {
			panic("can not find $GOPATH")
		}
		fmt.Fprintf(os.Stdout, "goproxy: %s inited.\n", time.Now().Format("2006-01-02 15:04:05"))
		gp := filepath.SplitList(gpEnv)
		gopath = gp[0]
	} else {
		gopath = confData.GoPath
		os.Setenv("GOPATH", gopath)
	}
	//gopath如果没有建一个
	if _, err := os.Stat(gopath); os.IsNotExist(err) {
		os.MkdirAll(gopath, 0755)
	}

	cacheDir = filepath.Join(gopath, "pkg", "mod", "cache", "download")
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stdout, "goproxy: %s cache dir is not exist. %s\n", time.Now().Format("2006-01-02 15:04:05"), cacheDir)
		//os.MkdirAll(cacheDir, 0644)
	}

	//一定要GOPROXY设置为空
	os.Setenv("GOPROXY", "")
	//读取配置

	http.Handle("/", mainHandler(http.FileServer(http.Dir(cacheDir))))
	err = http.ListenAndServe(confData.Listen, nil)
	if nil != err {
		panic(err)
	}
}

func mainHandler(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(os.Stdout, "goproxy: %s %s download %s\n", r.RemoteAddr, time.Now().Format("2006-01-02 15:04:05"), r.URL.Path)
		if _, err := os.Stat(filepath.Join(cacheDir, r.URL.Path)); err != nil {
			suffix := path.Ext(r.URL.Path)
			if suffix == ".info" || suffix == ".mod" || suffix == ".zip" {
				mod := strings.Split(r.URL.Path, "/@v/")
				if len(mod) != 2 {
					ReturnBadRequest(w, fmt.Errorf("bad module path:%s", r.URL.Path))
					return
				}
				version := strings.TrimSuffix(mod[1], suffix)
				version, err = module.DecodeVersion(version)
				if err != nil {
					ReturnServerError(w, err)
					return
				}
				path := strings.TrimPrefix(mod[0], "/")
				path, err := module.DecodePath(path)
				if err != nil {
					ReturnServerError(w, err)
					return
				}
				// ignore the error, incorrect tag may be given
				// forward to inner.ServeHTTP
				goGet(path, version, suffix, w, r)
			}

			// fetch latest version
			if strings.HasSuffix(r.URL.Path, "/@latest") {
				path := strings.TrimSuffix(r.URL.Path, "/@latest")
				path = strings.TrimPrefix(path, "/")
				path, err := module.DecodePath(path)
				if err != nil {
					ReturnServerError(w, err)
					return
				}
				goGet(path, "latest", "", w, r)
			}

			if strings.HasSuffix(r.URL.Path, "/@v/list") {
				w.Write([]byte(""))
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
}

func goGet(path, version, suffix string, w http.ResponseWriter, r *http.Request) error {
	//replace path
	rPath := path
	for originPath, destPath := range confData.MirrorMap {
		if strings.Index(path, originPath) != -1 {
			rPath = strings.Replace(path, originPath, destPath, 1)
			fmt.Fprintf(os.Stdout, "mirror path: %s mirror %s\n", path, rPath)
		}
	}
	cmd := exec.Command("go", "get", "-d", rPath+"@"+version)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	bytesErr, err := ioutil.ReadAll(stderr)
	if err != nil {
		return err
	}

	_, err = ioutil.ReadAll(stdout)
	if err != nil {
		return err
	}

	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "goproxy: download %s stderr:\n%s", rPath, string(bytesErr))
		return err
	}
	out := fmt.Sprintf("%s", bytesErr)

	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		if f[1] == "downloading" && f[2] == rPath && f[3] != version && suffix != "" {
			h := r.Host
			mod := strings.Split(r.URL.Path, "/@v/")
			p := fmt.Sprintf("%s/@v/%s%s", mod[0], f[3], suffix)
			scheme := "http:"
			if r.TLS != nil {
				scheme = "https:"
			}
			url := fmt.Sprintf("%s//%s/%s", scheme, h, p)
			http.Redirect(w, r, url, 302)
		}
	}
	if strings.Compare(rPath, path) != 0 {
		r1 := filepath.Join(confData.GoPath, "pkg", "mod", rPath)
		d1 := filepath.Join(confData.GoPath, "pkg", "mod", path)
		os.Link(r1, d1)
		fmt.Fprintf(os.Stdout, "link dir  %s -->  %s\n", r1, d1)
	}
	return nil
}
