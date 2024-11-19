package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"regexp"

	"github.com/CuteLicense/tts-server-go/tts/edge"
)

var (
	input     = flag.String("i", "", "Path to the .txt file (UTF-8 encoding)")
	output    = flag.String("o", "out.ogg", "Path to the output file (default 48kbps opus audio, only ogg/opus/webm are supported without -convert)")
	voice     = flag.String("voice", "zh-CN-XiaoxiaoNeural", "Voice selection")
	rate      = flag.String("rate", "1", "Speech rate")
	parallel  = flag.Uint("parallel", 1, "Max download threads (Max 8)")
	convert   = flag.Bool("convert", false, "Convert output format, requires ffmpeg")
)

var (
	useLocalFFmpeg  = false
	localFFmpegPath string
	wg              sync.WaitGroup
	tasks           chan Task
	totalLines      int
	execPath        string
	partPath        string
	partsFile       *os.File
)

type Task struct {
	line      int
	text      string
	storePath string
	savePath  string
}

func addTask(id int, text string) {
	text = strings.TrimSpace(text)
	if text == "" || !hasPronounceableCharacter(text)  {
		fmt.Printf("Finished: %v/%v (empty)\n", id, totalLines)
		return
	}
	fmt.Fprintf(partsFile, "file '%v'\n", fmt.Sprintf("%v.webm", id))
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s|%s|%s", *voice, *rate, text)))
	label := h.Sum(nil)
	storePath := fmt.Sprintf("%s/edgetts-store/%x", execPath, label)
	savePath := fmt.Sprintf("%v/%v.webm", partPath, id)
	if _, err := os.Stat(storePath); err == nil {
		os.Symlink(storePath, savePath)
		fmt.Printf("Finished: %v/%v (exist)\n", id, totalLines)
		return
	}
	wg.Add(1)
	tasks <- Task{id, text, storePath, savePath}
}

// hasPronounceableCharacter 检查字符串中是否包含可以发音的英文字符或中文字符。
func hasPronounceableCharacter(text string) bool {
	// 遍历字符串中的每个字符，检查是否至少有一个英文或中文字符
	for _, r := range text {
		if unicode.IsLetter(r) && (unicode.Is(unicode.Latin, r) || unicode.Is(unicode.Han, r)) {
			return true
		}
	}
	// 如果没有找到英文或中文字符，返回 false
	return false
}

func main() {
	flag.Parse()
	execPath, _ = os.Executable()
	execPath = filepath.Dir(execPath)

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		localFFmpegPath = execPath + ifElse(runtime.GOOS == "windows", "/ffmpeg-min.exe", "/ffmpeg-min")
		if _, err := os.Stat(localFFmpegPath); err == nil {
			useLocalFFmpeg = true
		} else {
			fmt.Println("ffmpeg not found")
			os.Exit(1)
		}
	}

	buf, err := os.ReadFile(*input)
	if err != nil {
		fmt.Println("Error reading input file:", err)
		os.Exit(1)
	}

	if *parallel == 0 || *parallel > 8 {
		fmt.Println("Parallel should be between 1-8")
		os.Exit(1)
	}

	if !utf8.Valid(buf) {
		fmt.Println("Invalid UTF-8 sequence")
		os.Exit(1)
	}

	if err := os.MkdirAll("edgetts-store", os.ModePerm); err != nil {
		fmt.Println("Error creating directory:", err)
		os.Exit(1)
	}

	paras := strings.Split(string(buf), ifElse(strings.Contains(string(buf), "\r\n"), "\r\n", "\n"))
	totalLines = len(paras)

	if *parallel > uint(totalLines) {
		*parallel = uint(totalLines)
	}

	partPath = fmt.Sprintf("%s.%x", filepath.Base(*input), time.Now().UnixNano())

	if err := os.MkdirAll(partPath, os.ModePerm); err != nil {
		fmt.Println("Error creating part directory:", err)
		os.Exit(1)
	}

	partsFile, err = os.Create(partPath + "/index")
	if err != nil {
		fmt.Println("Error creating index file:", err)
		os.Exit(1)
	}
	defer partsFile.Close()

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
	wg.Wait()
	close(tasks)

	var cmd *exec.Cmd
	ffmpegPath := ifElse(useLocalFFmpeg, localFFmpegPath, "ffmpeg")
	if *convert {
		cmd = exec.Command(ffmpegPath, "-y", "-f", "concat", "-i", partPath+"/index", *output)
	} else {
		cmd = exec.Command(ffmpegPath, "-y", "-f", "concat", "-i", partPath+"/index", "-c", "copy", *output)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Println("Error executing ffmpeg:", err)
	}
	fmt.Println(string(output))
}

func worker() {
	defer wg.Done()
	tts := &edge.TTS{}
	tts.NewConn()
	for {
		task, ok := <-tasks
		if !ok {
			return
		}
		ssml := fmt.Sprintf(`<speak xmlns="http://www.w3.org/2001/10/synthesis" xmlns:mstts="http://www.w3.org/2001/mstts" xmlns:emo="http://www.w3.org/2009/10/emotionml" version="1.0" xml:lang="en-US"><voice name="%s"><prosody rate="%s" pitch="+0Hz">%s</prosody></voice></speak>`, *voice, *rate, task.text)
		audioData, err := tts.GetAudio(ssml, "webm-24khz-16bit-mono-opus")
		for err != nil {
			fmt.Printf("Error: %v Retrying...\n", err)
			time.Sleep(3 * time.Second)
			audioData, err = tts.GetAudio(ssml, "webm-24khz-16bit-mono-opus")
		}
		os.WriteFile(task.storePath, audioData, 0666)
		os.Symlink(task.storePath, task.savePath)
		fmt.Printf("Finished: %v/%v\n", task.line, totalLines)
	}
}

func ifElse[T any](cond bool, trueVal, falseVal T) T {
	if cond {
		return trueVal
	}
	return falseVal
}
