package main

import (
	"bytes"
	"os"
	"strings"
	"time"
	"io"
	"bufio"
	"encoding/json"
	"fmt"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)

  scanner.Scan()
  input_json := scanner.Text()
	scanner.Scan()
	volume_name := scanner.Text()
	scanner.Scan()
	container_id := scanner.Text()

	volume := Volume{Name: volume_name}

	var reader io.Reader = strings.NewReader(input_json)

	// 標準出力を受け取る
	r, w, _ := os.Pipe()

	task, _ := NewTaskInfo("library-checker-images-python3", WithArguments("python3", "program.py"), WithStackLimitMB(-1), WithMemoryLimitMB(512), WithPidsLimit(100), WithWorkDir("/workdir"), WithVolume(&volume, "/workdir"), WithTimeout(2*time.Second), WithStdin(reader), WithStdout(w))

	ci := containerInfo{containerID: container_id}
	res, _ := task.start(ci)

	w.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r)
	s := strings.TrimRight(buf.String(), "\n")

	result := map[string]interface{}{
		"Output": s,
		"ExitCode": res.ExitCode,
		"Time": res.Time,
		"Memory": res.Memory,
		"TLE": res.TLE,
		"IA": false,
	}

	jsonStr, _ := json.Marshal(result)
	fmt.Println(string(jsonStr))
}
