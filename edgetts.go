package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/CuteLicense/tts-server-go/tts/edge"

)

var (
	input  = flag.String("i", "", "Path to the .txt file (UTF-8 encoding)")
	output = flag.String("o", "out.ogg", "Path to the output file (default 48kbps opus audio, only ogg/opus/webm are supported without -convert)")
	voice  = flag.String("voice", "zh-CN-XiaoxiaoNeural", `One of:
	en-US-AriaNeural
	en-US-JennyNeural
	en-US-GuyNeura
	en-US-SaraNeural
	ja-JP-NanamiNeural
	pt-BR-FranciscaNeural
	zh-CN-XiaoxiaoNeural
	zh-CN-YunyangNeural
	zh-CN-YunyeNeural
	zh-CN-YunxiNeural
	zh-CN-XiaohanNeural
	zh-CN-XiaomoNeural
	zh-CN-XiaoxuanNeural
	zh-CN-XiaoruiNeural
	zh-CN-XiaoshuangNeural
	... (other voice supported by edge TTS)
`)
	rate = flag.String("rate", "1", `One of:
	x-slow
	slow
	medium
	fast
	x-fast
	a rate number > 0 (meduim = 1)
	a delta number (+0.5, -0.2, ...)
`)
	parallel = flag.Uint("parallel", 1, "Max download threads (Max 8)")
	convert  = flag.Bool("convert", false, "Output with other formats like mp3/m4a/amr..., external ffmpeg is required")
)

var (
	signal          = struct{}{}
	useLocalFFmpeg  = false
	modified        = true
	localFFmpegPath string
	wg              sync.WaitGroup
	tasks           chan Task
	lines           int
	execPath        string
	partPath        string
	parts           *os.File
)

type Task struct {
	line                      int
	text, storePath, savePath string
}

func addTask(id int, text string) {
	if len(strings.TrimSpace(text)) == 0 {
		fmt.Printf("Finished: %v/%v (empty)\n", id, lines)
		return
	}
	fmt.Fprintf(parts, "file '%v'\n", fmt.Sprintf("%v.webm", id))
	h := sha256.New()
	h.Write(toBytes(fmt.Sprintf("%s|%s|", *voice, *rate)))
	h.Write(toBytes(text))
	label := h.Sum(nil)
	storePath := fmt.Sprintf("%s/edgetts-store/%x", execPath, label)
	savePath := fmt.Sprintf("%v/%v.webm", partPath, id)
	if _, err := os.Stat(storePath); err == nil {
		os.Symlink(storePath, savePath)
		fmt.Printf("Finished: %v/%v (exist)\n", id, lines)
		return
	}
	wg.Add(1)
	tasks <- Task{id, text, storePath, savePath}
}

func main() {
	flag.Parse()
	execPath, _ = os.Executable()
	execPath = filepath.Dir(execPath)

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		localFFmpegPath = execPath + If(runtime.GOOS == "windows", "/ffmpeg-min.exe", "/ffmpeg-min")
		if _, err := os.Stat(localFFmpegPath); err == nil {
			useLocalFFmpeg = true
			if *convert {
				println("external ffmpeg not found")
				os.Exit(1)
			}
		} else {
			println("ffmpeg not found")
			os.Exit(1)
		}
	}

	buf, err := os.ReadFile(*input)
	if err != nil {
		if input == nil || *input == "" {
			println("Use -h to get usage")
		} else {
			println(err.Error())
		}
		os.Exit(1)
	}

	if *parallel == 0 || *parallel > 8 {
		println("Parallel should between 1-8")
		os.Exit(1)
	}

	if !utf8.Valid(buf) {
		println("Invalid utf-8 sequence")
		os.Exit(1)
	}

	if err := os.MkdirAll("edgetts-store", os.ModePerm); err != nil {
		panic(err)
	}

	paras := strings.Split(toString(buf), If(strings.Contains(toString(buf), "\r\n"), "\r\n", "\n"))

	lines = len(paras)

	if *parallel > uint(lines) {
		*parallel = uint(lines)
	}

	partPath = fmt.Sprintf("%s.%x", filepath.Base(*input), time.Now().UnixNano())

	if err := os.MkdirAll(partPath, os.ModePerm); err != nil {
		panic(err)
	}

	parts, err = os.Create(partPath + "/index")
	if err != nil {
		panic(err)
	}

	tasks = make(chan Task, *parallel)

	for i := uint(0); i < *parallel; i++ {
		go worker()
	}

	for i, para := range paras {
		addTask(i+1, para)
		if *parallel == 1 {
			wg.Wait()
		}
	}
	parts.Close()
	wg.Wait()
	close(tasks)

	var cmd *exec.Cmd
	if *convert {
		cmd = exec.Command(If(useLocalFFmpeg, localFFmpegPath, "ffmpeg"), "-y", "-f", "concat", "-i", partPath+"/index", *output)
	} else {
		cmd = exec.Command(If(useLocalFFmpeg, localFFmpegPath, "ffmpeg"), "-y", "-f", "concat", "-i", partPath+"/index", "-c", "copy", *output)
	}
	output, _ := cmd.CombinedOutput()
	fmt.Println(toString(output))
}

func worker() {
	tts := &edge.TTS{}
	tts.NewConn()
	for {
		task, ok := <-tasks
		if !ok {
			return
		}
		ssml := `<speak xmlns="http://www.w3.org/2001/10/synthesis" xmlns:mstts="http://www.w3.org/2001/mstts" xmlns:emo="http://www.w3.org/2009/10/emotionml" version="1.0" xml:lang="en-US"><voice name="` + *voice + `"><prosody rate="` + *rate + `" pitch="+0Hz">` + task.text + `</prosody></voice></speak>`
		audioData, err := tts.GetAudio(ssml, "webm-24khz-16bit-mono-opus")
		for err != nil {
			fmt.Printf("Error: %v Retrying...\n", err)
			time.Sleep(time.Second * 3)
			audioData, err = tts.GetAudio(ssml, "webm-24khz-16bit-mono-opus")
		}
		os.WriteFile(task.storePath, audioData, 0666)
		os.Symlink(task.storePath, task.savePath)
		fmt.Printf("Finished: %v/%v\n", task.line, lines)
		wg.Done()
	}
}

func toString(bytes []byte) string {
	return *(*string)(unsafe.Pointer(&bytes))
}

func toBytes(s string) []byte {
	header := (*reflect.StringHeader)(unsafe.Pointer(&s))
	return *(*[]byte)(unsafe.Pointer(&reflect.SliceHeader{
		Data: header.Data,
		Len:  header.Len,
		Cap:  header.Len,
	}))
}

func If[T any](cond bool, trueVal, falseVal T) T {
	if cond {
		return trueVal
	}
	return falseVal
}
