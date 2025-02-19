package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	_ "github.com/lib/pq"
	"github.com/yosupo06/library-checker-judge/api/clientutil"
	pb "github.com/yosupo06/library-checker-judge/api/proto"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "google.golang.org/genproto/googleapis/cloud/secretmanager/v1"
)

// gRPC
var client pb.LibraryCheckerServiceClient
var judgeName string
var judgeCtx context.Context
var testCaseFetcher TestCaseFetcher

func execJudge(judgedir, testlibPath string, submissionID int32) (err error) {
	submission, err := client.SubmissionInfo(judgeCtx, &pb.SubmissionInfoRequest{
		Id: submissionID,
	})
	if err != nil {
		return err
	}
	problem, err := client.ProblemInfo(judgeCtx, &pb.ProblemInfoRequest{
		Name: submission.Overview.ProblemName,
	})
	log.Println("Submission info:", submissionID, submission.Overview.ProblemTitle)

	log.Println("Fetch data")
	if _, err = client.SyncJudgeTaskStatus(judgeCtx, &pb.SyncJudgeTaskStatusRequest{
		JudgeName:    judgeName,
		SubmissionId: submissionID,
		Status:       "Fetching",
	}); err != nil {
		return err
	}

	caseVersion := problem.CaseVersion
	testCases, err := testCaseFetcher.Fetch(submission.Overview.ProblemName, caseVersion)
	log.Print("Fetched :", caseVersion)
	if err != nil {
		log.Println("Fail to fetchData")
		return err
	}

	judge, err := NewJudge(judgedir, langs[submission.Overview.Lang], problem.TimeLimit)
	if err != nil {
		return err
	}
	defer judge.Close()

	defer func() {
		if err != nil {
			// error detected, try to change status into IE
			client.FinishJudgeTask(judgeCtx, &pb.FinishJudgeTaskRequest{
				JudgeName:    judgeName,
				SubmissionId: submissionID,
				Status:       "IE",
				CaseVersion:  caseVersion,
			})
		}
	}()

	if _, err = client.SyncJudgeTaskStatus(judgeCtx, &pb.SyncJudgeTaskStatusRequest{
		JudgeName:    judgeName,
		SubmissionId: submissionID,
		Status:       "Compiling",
	}); err != nil {
		return err
	}
	checkerFile, err := testCases.CheckerFile()
	if err != nil {
		return err
	}
	defer checkerFile.Close()

	testlib, err := os.Open(testlibPath)
	if err != nil {
		return err
	}
	taskResult, err := judge.CompileChecker(checkerFile, testlib)
	if err != nil {
		return err
	}
	if taskResult.ExitCode != 0 {
		if _, err = client.FinishJudgeTask(judgeCtx, &pb.FinishJudgeTaskRequest{
			JudgeName:    judgeName,
			SubmissionId: submissionID,
			Status:       "ICE",
			CaseVersion:  caseVersion,
		}); err != nil {
			return err
		}
		return nil
	}

	tmpSourceFile, err := os.CreateTemp("", "output-")
	if err != nil {
		return err
	}
	defer os.Remove(tmpSourceFile.Name())

	if _, err := tmpSourceFile.WriteString(submission.Source); err != nil {
		return err
	}
	tmpSourceFile.Close()

	tmpSourceFile2, err := os.Open(tmpSourceFile.Name())
	if err != nil {
		return err
	}
	defer tmpSourceFile2.Close()

	result, compileError, err := judge.CompileSource(tmpSourceFile2)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		if _, err = client.SyncJudgeTaskStatus(judgeCtx, &pb.SyncJudgeTaskStatusRequest{
			JudgeName:    judgeName,
			SubmissionId: submissionID,
			CompileError: compileError,
			Status:       "CE",
		}); err != nil {
			return err
		}

		if _, err = client.FinishJudgeTask(judgeCtx, &pb.FinishJudgeTaskRequest{
			JudgeName:    judgeName,
			SubmissionId: submissionID,
			CaseVersion:  caseVersion,
		}); err != nil {
			return err
		}
		return nil
	}
	if _, err = client.SyncJudgeTaskStatus(judgeCtx, &pb.SyncJudgeTaskStatusRequest{
		JudgeName:    judgeName,
		SubmissionId: submissionID,
		Status:       "Executing",
	}); err != nil {
		return err
	}

	cases, err := testCases.CaseNames()
	if err != nil {
		return err
	}
	unsendCases := []CaseResult{}
	sendCase := func() error {
		cases := []*pb.SubmissionCaseResult{}
		for _, caseResult := range unsendCases {
			cases = append(cases, &pb.SubmissionCaseResult{
				Case:   caseResult.CaseName,
				Status: caseResult.Status,
				Time:   caseResult.Time.Seconds(),
				Memory: int64(caseResult.Memory),
			})
		}
		if _, err = client.SyncJudgeTaskStatus(judgeCtx, &pb.SyncJudgeTaskStatusRequest{
			JudgeName:    judgeName,
			SubmissionId: submissionID,
			Status:       "Executing",
			CaseResults:  cases,
		}); err != nil {
			return err
		}
		unsendCases = []CaseResult{}
		return nil
	}
	caseResults := []CaseResult{}
	lastSend := time.Time{}
	addCase := func(caseResult *CaseResult) error {
		if caseResult != nil {
			caseResults = append(caseResults, *caseResult)
			unsendCases = append(unsendCases, *caseResult)
		}
		if lastSend.Add(time.Second).Before(time.Now()) {
			lastSend = time.Now()
			if err := sendCase(); err != nil {
				return err
			}
		}
		return nil
	}
	for _, caseName := range cases {
		inFile, err := testCases.InFile(caseName)
		if err != nil {
			return err
		}
		outFile, err := testCases.OutFile(caseName)
		if err != nil {
			return err
		}
		caseResult, err := judge.TestCase(inFile, outFile)
		if err != nil {
			return err
		}
		caseResult.CaseName = caseName
		if err := addCase(&caseResult); err != nil {
			return err
		}
	}
	if err := sendCase(); err != nil {
		return err
	}
	caseResult := AggregateResults(caseResults)
	if _, err = client.FinishJudgeTask(judgeCtx, &pb.FinishJudgeTaskRequest{
		JudgeName:    judgeName,
		SubmissionId: submissionID,
		Status:       caseResult.Status,
		Time:         caseResult.Time.Seconds(),
		Memory:       int64(caseResult.Memory),
		CaseVersion:  caseVersion,
	}); err != nil {
		return err
	}
	return nil
}

func initClient(conn *grpc.ClientConn, apiUser, apiPassword string) {
	client = pb.NewLibraryCheckerServiceClient(conn)
	ctx := context.Background()
	resp, err := client.Login(ctx, &pb.LoginRequest{
		Name:     apiUser,
		Password: apiPassword,
	})

	if err != nil {
		log.Fatal("Cannot login to API Server:", err)
	}
	judgeCtx = clientutil.ContextWithToken(ctx, resp.Token)

	judgeName, err = os.Hostname()
	if err != nil {
		log.Fatal("Cannot get hostname:", err)
	}
	log.Print("JudgeName: ", judgeName)
}

func apiConnect(apiHost string, useTLS bool) *grpc.ClientConn {
	options := []grpc.DialOption{grpc.WithBlock(), grpc.WithPerRPCCredentials(&clientutil.LoginCreds{}), grpc.WithTimeout(10 * time.Second)}
	if !useTLS {
		log.Print("local mode")
		options = append(options, grpc.WithInsecure())
	} else {
		systemRoots, err := x509.SystemCertPool()
		if err != nil {
			log.Fatal(err)
		}
		creds := credentials.NewTLS(&tls.Config{
			RootCAs: systemRoots,
		})
		options = append(options, grpc.WithTransportCredentials(creds))
	}
	log.Printf("Connect to API host: %v", apiHost)
	conn, err := grpc.Dial(apiHost, options...)
	if err != nil {
		log.Fatal("Cannot connect to the API server:", err)
	}
	return conn
}

func getSecureString(secureKey, defaultValue string) string {
	if secureKey == "" {
		if defaultValue == "" {
			log.Fatal("both secureKey and defaultValue is empty")
		}
		return defaultValue
	}

	// Create the client.
	ctx := context.Background()
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		log.Fatalf("failed to create secretmanager client: %v", err)
	}
	defer client.Close()

	// Build the request.
	req := &secretmanagerpb.AccessSecretVersionRequest{
		Name: secureKey,
	}

	// Call the API.
	result, err := client.AccessSecretVersion(ctx, req)
	if err != nil {
		log.Fatalf("failed to access secret version: %v", err)
	}

	return string(result.Payload.Data)
}

func main() {
	testlibPath := flag.String("testlib", "sources/testlib.h", "path of testlib.h")
	langsTomlPath := flag.String("langs", "../langs/langs.toml", "toml path of langs.toml")
	judgedir := flag.String("judgedir", "", "temporary directory of judge")

	prod := flag.Bool("prod", false, "production mode")

	minioHost := flag.String("miniohost", "localhost:9000", "minio host")
	minioHostSecret := flag.String("miniohost-secret", "", "gcloud secret of minio host")

	minioID := flag.String("minioid", "minio", "minio ID")
	minioIDSecret := flag.String("minioid-secret", "", "gcloud secret of minio ID")

	minioKey := flag.String("miniokey", "miniopass", "minio access key")
	minioKeySecret := flag.String("miniokey-secret", "", "gcloud secret of minio access key")

	minioBucket := flag.String("miniobucket", "testcase", "minio bucket")
	minioBucketSecret := flag.String("miniobucket-secret", "", "gcloud secret of minio bucket")

	apiHost := flag.String("apihost", "localhost:50051", "api host")

	apiUser := flag.String("apiuser", "judge", "api user")

	apiPass := flag.String("apipass", "password", "api password")
	apiPassSecret := flag.String("apipass-secret", "", "gcloud secret of api password")

	flag.Parse()

	ReadLangs(*langsTomlPath)

	var err error

	// init gRPC
	conn := apiConnect(*apiHost, *prod)
	defer conn.Close()
	initClient(conn, *apiUser, getSecureString(*apiPassSecret, *apiPass))

	testCaseFetcher, err = NewTestCaseFetcher(
		getSecureString(*minioHostSecret, *minioHost),
		getSecureString(*minioIDSecret, *minioID),
		getSecureString(*minioKeySecret, *minioKey),
		getSecureString(*minioBucketSecret, *minioBucket),
		*prod,
	)
	if err != nil {
		log.Fatal(err)
	}
	defer testCaseFetcher.Close()

	log.Println("Start Pooling")
	for {
		task, err := client.PopJudgeTask(judgeCtx, &pb.PopJudgeTaskRequest{
			JudgeName: judgeName,
		})
		if err != nil {
			time.Sleep(3 * time.Second)
			log.Print("PopJudgeTask error: ", err)
			continue
		}
		if task.SubmissionId == -1 {
			time.Sleep(3 * time.Second)
			continue
		}
		log.Println("Start Judge:", task.SubmissionId)
		err = execJudge(*judgedir, *testlibPath, task.SubmissionId)
		if err != nil {
			log.Println(err.Error())
			continue
		}
	}
}
