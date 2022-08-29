package main

import (
	"os"
	"bufio"
	"embed"
	"time"
	"encoding/json"
	"fmt"
)

//go:embed programs/*
var sources embed.FS


func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	uid := scanner.Text()

	volume, _ := CreateVolume()
	src, _ := sources.Open("programs/" + uid + ".py")
	volume.CopyFile(src, "program.py")

	task, _ := NewTaskInfo("library-checker-images-python3", WithArguments("python3", "program.py"), WithStackLimitMB(-1), WithMemoryLimitMB(512), WithPidsLimit(100), WithWorkDir("/workdir"), WithVolume(&volume, "/workdir"), WithTimeout(2*time.Second))
	ci, _ := task.create()

	result := map[string]interface{}{
		"volume_name": volume.Name,
		"container_id": ci.containerID,
	}

	jsonStr, _ := json.Marshal(result)
	fmt.Println(string(jsonStr))
}
