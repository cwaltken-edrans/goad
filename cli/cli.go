package cli

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/signal"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/alecthomas/kingpin.v2"
	ini "gopkg.in/ini.v1"

	"github.com/dustin/go-humanize"
	"github.com/goadapp/goad/goad"
	"github.com/goadapp/goad/queue"
	"github.com/goadapp/goad/version"
	"github.com/nsf/termbox-go"
)

var (
	app             = kingpin.New("goad", "An AWS Lambda powered load testing tool")
	urlArg          = app.Arg("url", "[http[s]://]hostname[:port]/path optional if defined in goad.ini")
	url             = urlArg.String()
	requestsFlag    = app.Flag("requests", "Number of requests to perform. Set to 0 in combination with a specified timelimit allows for unlimited requests for the specified time.").Short('n').Default("1000")
	requests        = requestsFlag.Int()
	concurrencyFlag = app.Flag("concurrency", "Number of multiple requests to make at a time").Short('c').Default("10")
	concurrency     = concurrencyFlag.Int()
	timelimitFlag   = app.Flag("timelimit", "Seconds to max. to spend on benchmarking").Short('t').Default("3600")
	timelimit       = timelimitFlag.Int()
	timeoutFlag     = app.Flag("timeout", "Seconds to max. wait for each response").Short('s').Default("15")
	timeout         = timeoutFlag.Int()
	headersFlag     = app.Flag("header", "Add Arbitrary header line, eg. 'Accept-Encoding: gzip' (repeatable)").Short('H')
	headers         = headersFlag.Strings()
	regionsFlag     = app.Flag("region", "AWS regions to run in. Repeat flag to run in more then one region. (repeatable)").HintOptions(goad.SupportedRegions()...)
	regions         = regionsFlag.Strings()
	outputFileFlag  = app.Flag("output-json", "Optional path to file for JSON result storage")
	outputFile      = outputFileFlag.String()
	methodFlag      = app.Flag("method", "HTTP method").Short('m').Default("GET")
	method          = methodFlag.String()
	bodyFlag        = app.Flag("body", "HTTP request body")
	body            = bodyFlag.String()
)

const coldef = termbox.ColorDefault
const nano = 1000000000

func Run() {
	app.HelpFlag.Short('h')
	app.Version(version.String())
	app.VersionFlag.Short('V')

	config := aggregateConfiguration()
	test := createGoadTest(config)

	var finalResult queue.RegionsAggData
	defer printSummary(&finalResult)

	if config.Output != "" {
		defer saveJSONSummary(*outputFile, &finalResult)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM) // but interrupts from kbd are blocked by termbox

	start(test, &finalResult, sigChan)
}

func aggregateConfiguration() *goad.TestConfig {
	config := parseSettingsFile("goad.ini")
	applyDefaultsFromConfig(config)
	return parseCommandline()
}

func applyDefaultsFromConfig(config *goad.TestConfig) {
	applyDefaultIfNotZero(bodyFlag, config.Body)
	applyDefaultIfNotZero(concurrencyFlag, prepareInt(config.Concurrency))
	applyDefaultIfNotZero(headersFlag, config.Headers)
	applyDefaultIfNotZero(methodFlag, config.Method)
	applyDefaultIfNotZero(outputFileFlag, config.Output)
	applyDefaultIfNotZero(regionsFlag, config.Regions)
	applyDefaultIfNotZero(requestsFlag, prepareInt(config.Requests))
	applyDefaultIfNotZero(timelimitFlag, prepareInt(config.Timelimit))
	applyDefaultIfNotZero(timeoutFlag, prepareInt(config.Timeout))
	if config.URL == "" {
		urlArg.Required()
	} else {
		urlArg.Default(config.URL)
	}
	if len(config.Regions) == 0 {
		regionsFlag.Default("us-east-1", "eu-west-1", "ap-northeast-1")
	}
}

func applyDefaultIfNotZero(flag *kingpin.FlagClause, def interface{}) {
	value := reflect.ValueOf(def)
	kind := value.Kind()
	if isNotZero(value) {
		if kind == reflect.Slice || kind == reflect.Array {
			strs := make([]string, 0)
			for i := 0; i < value.Len(); i++ {
				strs = append(strs, value.Index(i).String())
			}
			flag.Default(strs...)
		} else {
			flag.Default(value.String())
		}
	}
}

func prepareInt(value int) string {
	if value == 0 {
		return ""
	}
	return strconv.Itoa(value)
}

func isNotZero(v reflect.Value) bool {
	return !isZero(v)
}

func isZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Func, reflect.Map, reflect.Slice:
		return v.IsNil()
	case reflect.Array:
		z := true
		for i := 0; i < v.Len(); i++ {
			z = z && isZero(v.Index(i))
		}
		return z
	case reflect.Struct:
		z := true
		for i := 0; i < v.NumField(); i++ {
			z = z && isZero(v.Field(i))
		}
		return z
	}
	// Compare other types directly:
	z := reflect.Zero(v.Type())
	return v.Interface() == z.Interface()
}

func parseSettingsFile(file string) *goad.TestConfig {
	config := &goad.TestConfig{}
	cfg, err := ini.LoadSources(ini.LoadOptions{AllowBooleanKeys: true}, "goad.ini")
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Println(err.Error())
		}
		return config
	}
	generalSection := cfg.Section("general")
	config.URL = generalSection.Key("url").String()
	config.Method = generalSection.Key("method").String()
	config.Body = generalSection.Key("body").String()
	config.Concurrency, _ = generalSection.Key("concurrency").Int()
	config.Requests, _ = generalSection.Key("requests").Int()
	config.Timelimit, _ = generalSection.Key("timelimit").Int()
	config.Timeout, _ = generalSection.Key("timeout").Int()
	config.Output = generalSection.Key("json-output").String()

	regionsSection := cfg.Section("regions")
	config.Regions = regionsSection.KeyStrings()

	headersSection := cfg.Section("headers")
	headerHash := headersSection.KeysHash()
	config.Headers = foldHeaders(headerHash)
	return config
}

func foldHeaders(hash map[string]string) []string {
	headersList := make([]string, 0)
	for k, v := range hash {
		headersList = append(headersList, fmt.Sprintf("%s: %s", k, v))
	}
	return headersList
}

func parseCommandline() *goad.TestConfig {
	args := os.Args[1:]

	_, err := app.Parse(args)
	if err != nil {
		fmt.Println(err.Error())
		app.Usage(args)
		os.Exit(1)
	}

	regionsArray := parseRegionsForBackwardsCompatibility(*regions)

	config := &goad.TestConfig{}
	config.URL = *url
	config.Concurrency = *concurrency
	config.Requests = *requests
	config.Timelimit = *timelimit
	config.Timeout = *timeout
	config.Regions = regionsArray
	config.Method = *method
	config.Body = *body
	config.Headers = *headers
	config.Output = *outputFile
	return config
}

func parseRegionsForBackwardsCompatibility(regions []string) []string {
	parsedRegions := make([]string, 0)
	for _, str := range regions {
		parsedRegions = append(parsedRegions, strings.Split(str, ",")...)
	}
	return parsedRegions
}

func createGoadTest(config *goad.TestConfig) *goad.Test {
	test, err := goad.NewTest(config)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	return test
}

func start(test *goad.Test, finalResult *queue.RegionsAggData, sigChan chan os.Signal) {
	err := termbox.Init()
	if err != nil {
		panic(err)
	}

	defer test.Clean()
	defer termbox.Close()
	termbox.Sync()
	renderString(0, 0, "Launching on AWS... (be patient)", coldef, coldef)
	renderLogo()
	termbox.Flush()

	resultChan := test.Start()

	_, h := termbox.Size()
	renderString(0, h-1, "Press ctrl-c to interrupt", coldef, coldef)
	termbox.Flush()

	go func() {
		for {
			event := termbox.PollEvent()
			if event.Key == 3 {
				sigChan <- syscall.SIGINT
			}
		}
	}()

	startTime := time.Now()
	firstTime := true
outer:
	for {
		select {
		case result, ok := <-resultChan:
			if !ok {
				break outer
			}

			if firstTime {
				clearLogo()
				firstTime = false
			}

			// sort so that regions always appear in the same order
			var regions []string
			for key := range result.Regions {
				regions = append(regions, key)
			}
			sort.Strings(regions)
			y := 3
			totalReqs := 0
			for _, region := range regions {
				data := result.Regions[region]
				totalReqs += data.TotalReqs
				y = renderRegion(data, y)
				y++
			}

			y = 0
			var percentDone float64
			if result.TotalExpectedRequests > 0 {
				percentDone = float64(totalReqs) / float64(result.TotalExpectedRequests)
			} else {
				percentDone = math.Min(float64(time.Since(startTime).Seconds())/float64(test.Config.Timelimit), 1.0)
			}
			drawProgressBar(percentDone, y)

			termbox.Flush()
			finalResult.Regions = result.Regions

		case <-sigChan:
			break outer
		}
	}
}

func renderLogo() {
	s1 := `	  _____                 _`
	s2 := `  / ____|               | |`
	s3 := `	| |  __  ___   ____  __| |`
	s4 := `	| | |_ |/ _ \ / _  |/ _  |`
	s5 := `	| |__| | (_) | (_| | (_| |`
	s6 := `	 \_____|\___/ \__,_|\__,_|`
	s7 := " Global load testing with Go"
	arr := [...]string{s1, s2, s3, s4, s5, s6, s7}
	for i, str := range arr {
		renderString(0, i+1, str, coldef, coldef)
	}
}

// Also clears loading message
func clearLogo() {
	for i := 0; i < 8; i++ {
		renderString(0, i, "                                ", coldef, coldef)
	}
}

// renderRegion returns the y for the next empty line
func renderRegion(data queue.AggData, y int) int {
	x := 0
	renderString(x, y, "Region: ", termbox.ColorWhite, termbox.ColorBlue)
	x += 8
	regionStr := fmt.Sprintf("%s", data.Region)
	renderString(x, y, regionStr, termbox.ColorWhite|termbox.AttrBold, termbox.ColorBlue)
	x = 0
	y++
	headingStr := "   TotReqs   TotBytes    AvgTime   AvgReq/s  AvgKbps/s"
	renderString(x, y, headingStr, coldef|termbox.AttrBold, coldef)
	y++
	resultStr := fmt.Sprintf("%10d %10s   %7.3fs %10.2f %10.2f", data.TotalReqs, humanize.Bytes(uint64(data.TotBytesRead)), float64(data.AveTimeForReq)/nano, data.AveReqPerSec, data.AveKBytesPerSec)
	renderString(x, y, resultStr, coldef, coldef)
	y++
	headingStr = "   Slowest    Fastest   Timeouts  TotErrors"
	renderString(x, y, headingStr, coldef|termbox.AttrBold, coldef)
	y++
	resultStr = fmt.Sprintf("  %7.3fs   %7.3fs %10d %10d", float64(data.Slowest)/nano, float64(data.Fastest)/nano, data.TotalTimedOut, totErrors(&data))
	renderString(x, y, resultStr, coldef, coldef)
	y++

	return y
}

func totErrors(data *queue.AggData) int {
	var okReqs int
	for statusStr, value := range data.Statuses {
		status, _ := strconv.Atoi(statusStr)
		if status < 400 {
			okReqs += value
		}
	}
	return data.TotalReqs - okReqs
}

func drawProgressBar(percent float64, y int) {
	x := 0
	width := 52
	percentStr := fmt.Sprintf("%5.1f%%            ", percent*100)
	renderString(x, y, percentStr, coldef, coldef)
	y++
	hashes := int(percent * float64(width))
	if percent > 0.99 {
		hashes = width
	}
	renderString(x, y, "[", coldef, coldef)

	for x++; x <= hashes; x++ {
		renderString(x, y, "#", coldef, coldef)
	}
	renderString(width+1, y, "]", coldef, coldef)
}

func renderString(x int, y int, str string, f termbox.Attribute, b termbox.Attribute) {
	for i, c := range str {
		termbox.SetCell(x+i, y, c, f, b)
	}
}

func boldPrintln(msg string) {
	fmt.Printf("\033[1m%s\033[0m\n", msg)
}

func printData(data *queue.AggData) {
	boldPrintln("   TotReqs   TotBytes    AvgTime   AvgReq/s  AvgKbps/s")
	fmt.Printf("%10d %10s   %7.3fs %10.2f %10.2f\n", data.TotalReqs, humanize.Bytes(uint64(data.TotBytesRead)), float64(data.AveTimeForReq)/nano, data.AveReqPerSec, data.AveKBytesPerSec)
	boldPrintln("   Slowest    Fastest   Timeouts  TotErrors")
	fmt.Printf("  %7.3fs   %7.3fs %10d %10d", float64(data.Slowest)/nano, float64(data.Fastest)/nano, data.TotalTimedOut, totErrors(data))
	fmt.Println("")
}

func printSummary(result *queue.RegionsAggData) {
	if len(result.Regions) == 0 {
		boldPrintln("No results received")
		return
	}
	boldPrintln("Regional results")
	fmt.Println("")

	for region, data := range result.Regions {
		fmt.Println("Region: " + region)
		printData(&data)
	}

	overall := queue.SumRegionResults(result)

	fmt.Println("")
	boldPrintln("Overall")
	fmt.Println("")
	printData(overall)

	boldPrintln("HTTPStatus   Requests")
	for statusStr, value := range overall.Statuses {
		fmt.Printf("%10s %10d\n", statusStr, value)
	}
	fmt.Println("")
}

func saveJSONSummary(path string, result *queue.RegionsAggData) {
	if len(result.Regions) == 0 {
		return
	}
	results := make(map[string]queue.AggData)

	for region, data := range result.Regions {
		results[region] = data
	}

	overall := queue.SumRegionResults(result)

	results["overall"] = *overall
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		fmt.Println(err)
		return
	}
	err = ioutil.WriteFile(path, b, 0644)
	if err != nil {
		fmt.Println(err)
		return
	}
}
