package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	sheets "google.golang.org/api/sheets/v4"
)

func getVariableOrDefault(varName, defaultValue string) string {
	varValue := os.Getenv(varName)
	if len(varValue) > 0 {
		return varValue
	}
	return defaultValue
}

var clientSecretFile = getVariableOrDefault("CLIENT_SECRET_FILE", "client_secret.json")
var cachedCredsFile = getVariableOrDefault("CACHED_CREDS_FILE", "blackbox.creds.json")

const spreadsheetsScope = "https://www.googleapis.com/auth/spreadsheets"

func auth() (*sheets.Service, error) {
	ctx := context.Background()
	b, err := ioutil.ReadFile(clientSecretFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to read client secret file: %v", err)
	}

	config, err := google.ConfigFromJSON(b, spreadsheetsScope)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse client secret file to config: %v", err)
	}
	client, err := getClient(ctx, config)
	if err != nil {
		return nil, err
	}

	return sheets.New(client)
}

func getClient(ctx context.Context, config *oauth2.Config) (*http.Client, error) {
	tok, err := tokenFromFile(cachedCredsFile)
	if err != nil {
		tok, err = getTokenFromWeb(config)
		if err != nil {
			return nil, err
		}
		saveToken(cachedCredsFile, tok)
	}
	return config.Client(ctx, tok), nil
}

// getTokenFromWeb uses Config to request a Token.
// It returns the retrieved Token.
func getTokenFromWeb(config *oauth2.Config) (*oauth2.Token, error) {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return nil, fmt.Errorf("Unable to read authorization code %v", err)
	}

	tok, err := config.Exchange(oauth2.NoContext, code)
	if err != nil {
		return nil, fmt.Errorf("Unable to retrieve token from web %v", err)
	}
	return tok, nil
}

// tokenFromFile retrieves a Token from a given file path.
// It returns the retrieved Token and any read error encountered.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	t := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(t)
	defer f.Close()
	return t, err
}

// saveToken uses a file path to create a file and store the
// token in it.
func saveToken(file string, token *oauth2.Token) error {
	fmt.Printf("Saving credential file to: %s\n", file)
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
	return nil
}

func ReadSetupRows(service *sheets.Service, spreadsheetID, setupSheetName string) ([][]string, error) {
	readRange := setupSheetName + "!A1:Z"
	rows := [][]string{}

	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, readRange).Do()
	if err != nil {
		return rows, fmt.Errorf("Unable to retrieve data from sheet. %v", err)
	}

	if len(resp.Values) > 0 {
		for _, row := range resp.Values {
			stringRow := []string{}
			for _, item := range row {
				stringRow = append(stringRow, item.(string))
			}
			rows = append(rows, stringRow)
		}
	} else {
		return rows, fmt.Errorf("No data found.")
	}

	return rows, nil
}

func ExtractExamples(examplesCell string) []string {
	examples := []string{}
	for _, example := range strings.Split(examplesCell, ",") {
		trimmed := strings.Trim(example, " \t")
		if trimmed != "" {
			examples = append(examples, trimmed)
		}
	}
	return examples
}

func GetInputSets(exampleSets [][]string) [][]string {
	result := [][]string{}
	if len(exampleSets) == 0 {
		return result
	}
	if len(exampleSets) == 1 {
		for _, item := range exampleSets[0] {
			result = append(result, []string{item})
		}
	}
	head := exampleSets[0]
	tail := exampleSets[1:]
	for _, item := range head {
		for _, subitem := range GetInputSets(tail) {
			result = append(result, append([]string{item}, subitem...))
		}
	}
	return result
}

func GetVarsExamplesSets(setupRows [][]string) ([]string, [][]string, error) {
	vars := []string{}
	examplesSets := [][]string{}

	for i, setupRow := range setupRows {
		varCell := setupRow[0]
		examplesCell := setupRow[1]
		varName := strings.Trim(varCell, "\t \n")
		if varName == "" {
			return vars, examplesSets, fmt.Errorf("Could not extract var name from row %d", i)
		}
		examples := ExtractExamples(examplesCell)
		if len(examples) == 0 {
			return vars, examplesSets, fmt.Errorf("Could not extract examples from row %d", i)
		}
		vars = append(vars, varName)
		examplesSets = append(examplesSets, examples)
	}

	return vars, examplesSets, nil
}

func RecordSortedKeys(output map[string]string) []string {
	keys := make([]string, 0)
	for key, _ := range output {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func RunBlackBoxCmd(progPath string, varNames, inputSet []string) (map[string]string, error) {
	inputMap := make(map[string]string)
	for i, inputItem := range inputSet {
		inputMap[varNames[i]] = inputItem
	}
	// Marshal into JSON
	jsonBytes, err := json.Marshal(inputMap)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(progPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	go func() {
		stdin.Write(jsonBytes)
		defer stdin.Close()
	}()
	// Read output

	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	// Unmarshal output
	outputMap := make(map[string]string)
	err = json.Unmarshal(output, &outputMap)
	return outputMap, err
}

func RecordResults(srv *sheets.Service, spreadsheetID, resultSheetName string, varNames []string, resultChannel chan []string) error {
	currentLine := 1
	getAddress := func() string {
		return fmt.Sprintf("%s!A%d", resultSheetName, currentLine)
	}
	// While info is coming from the channel, keep updating rows
	for resultLine := range resultChannel {
		resultRow := make([]interface{}, 0)
		for _, input := range resultLine {
			resultRow = append(resultRow, input)
		}

		vr := sheets.ValueRange{
			Values: [][]interface{}{resultRow},
		}

		_, err := srv.Spreadsheets.Values.Update(spreadsheetID, getAddress(), &vr).ValueInputOption("USER_ENTERED").Do()
		if err != nil {
			return err
		}
		currentLine++
	}
	return nil
}

func RunExploration(progPath string, varNames []string, inputSets [][]string, resultChan chan []string) error {
	outputVars := []string{}

	for i, inputSet := range inputSets {
		fmt.Fprintf(os.Stderr, " ===> [%d/%d] <===", i+1, len(inputSets))
		outputMap, err := RunBlackBoxCmd(progPath, varNames, inputSet)
		fmt.Fprintf(os.Stderr, "\r")

		if err != nil {
			return err
		}
		if len(outputVars) == 0 {
			outputVars = RecordSortedKeys(outputMap)
			// Send the header
			resultChan <- append(varNames, outputVars...)
		}
		resultLine := append([]string{}, inputSet...)
		for _, outputVar := range outputVars {
			resultLine = append(resultLine, outputMap[outputVar])
		}
		resultChan <- resultLine
	}
	return nil
}

func CreateNewResultSheet(srv *sheets.Service, spreadsheetID, sheetName string) error {

	addRequest := sheets.Request{}

	requestsString := fmt.Sprintf(`{
      "addSheet": {
        "properties": {
          "title": "%s",
          "tabColor": {
            "red": 1.0,
            "green": 0.3,
            "blue": 0.4
          }
        }
      }
	}`, sheetName)
	err := json.Unmarshal([]byte(requestsString), &addRequest)
	if err != nil {
		return err
	}

	rb := &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{&addRequest},
	}

	_, err = srv.Spreadsheets.BatchUpdate(spreadsheetID, rb).Do()
	if err != nil {
		return err
	}

	return nil
}

func main() {
	fmt.Println("blackbox\n========")
	// Read the spreadsheet
	//   take the id of the spreadsheet
	if len(os.Args) < 3 {
		panic("spreadsheet or progpath param is missing")
	}

	spreadsheetId := os.Args[1]
	progPath := os.Args[2]
	fmt.Println(spreadsheetId, progPath)
	//   authenticate
	srv, err := auth()
	if err != nil {
		panic(err)
	}

	// retreive data from spreadsheet/inputs
	setupRows, err := ReadSetupRows(srv, spreadsheetId, "inputs")
	if err != nil {
		panic(err)
	}

	// Create cartesian product from the inputs
	varNames, exampleSets, err := GetVarsExamplesSets(setupRows)
	if err != nil {
		panic(err)
	}
	inputSets := GetInputSets(exampleSets)
	log.Printf("Got %d input sets for %d variables\n", len(inputSets), len(varNames))

	resultSheetName := fmt.Sprintf("result_%d", time.Now().Unix())
	err = CreateNewResultSheet(srv, spreadsheetId, resultSheetName)
	if err != nil {
		panic(err)
	}

	resultChannel := make(chan []string)
	defer close(resultChannel)
	recordErrorChannel := make(chan error)
	defer close(recordErrorChannel)
	exploreErrorChannel := make(chan error)
	defer close(exploreErrorChannel)

	go func() {
		recordErrorChannel <- RecordResults(srv, spreadsheetId, resultSheetName, varNames, resultChannel)
	}()

	go func() {
		exploreErrorChannel <- RunExploration(progPath, varNames, inputSets, resultChannel)
	}()

	for {
		select {
		case err := <-recordErrorChannel:
			if err != nil {
				panic(err)
			}
		case err := <-exploreErrorChannel:
			if err != nil {
				panic(err)
			}
			return
		}
	}
}
