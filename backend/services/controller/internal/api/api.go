package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/leandrofars/oktopus/internal/api/auth"
	"github.com/leandrofars/oktopus/internal/api/cors"
	"github.com/leandrofars/oktopus/internal/api/middleware"
	"github.com/leandrofars/oktopus/internal/db"
	"github.com/leandrofars/oktopus/internal/mtp"
	usp_msg "github.com/leandrofars/oktopus/internal/usp_message"
	"github.com/leandrofars/oktopus/internal/utils"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/protobuf/proto"
)

type Api struct {
	Port     string
	Db       db.Database
	Broker   mtp.Broker
	MsgQueue map[string](chan usp_msg.Msg)
	QMutex   *sync.Mutex
}

type WiFi struct {
	SSID                 string   `json:"ssid"`
	Password             string   `json:"password"`
	Security             string   `json:"security"`
	SecurityCapabilities []string `json:"securityCapabilities"`
	AutoChannelEnable    bool     `json:"autoChannelEnable"`
	Channel              int      `json:"channel"`
	ChannelBandwidth     string   `json:"channelBandwidth"`
	FrequencyBand        string   `json:"frequencyBand"`
	//PossibleChannels     		[]int    `json:"PossibleChannels"`
	SupportedChannelBandwidths []string `json:"supportedChannelBandwidths"`
}

const (
	NormalUser = iota
	AdminUser
)

func NewApi(port string, db db.Database, b mtp.Broker, msgQueue map[string](chan usp_msg.Msg), m *sync.Mutex) Api {
	return Api{
		Port:     port,
		Db:       db,
		Broker:   b,
		MsgQueue: msgQueue,
		QMutex:   m,
	}
}

//TODO: restructure http api calls for mqtt, to use golang generics and avoid code repetition
//TODO: standardize timeouts through code
//TODO: fix api methods

func StartApi(a Api) {
	r := mux.NewRouter()
	authentication := r.PathPrefix("/api/auth").Subrouter()
	authentication.HandleFunc("/login", a.generateToken).Methods("PUT")
	authentication.HandleFunc("/register", a.registerUser).Methods("POST")
	authentication.HandleFunc("/admin/register", a.registerAdminUser).Methods("POST")
	authentication.HandleFunc("/admin/exists", a.adminUserExists).Methods("GET")
	iot := r.PathPrefix("/api/device").Subrouter()
	iot.HandleFunc("", a.retrieveDevices).Methods("GET")
	iot.HandleFunc("/{sn}/get", a.deviceGetMsg).Methods("PUT")
	iot.HandleFunc("/{sn}/add", a.deviceCreateMsg).Methods("PUT")
	iot.HandleFunc("/{sn}/del", a.deviceDeleteMsg).Methods("PUT")
	iot.HandleFunc("/{sn}/set", a.deviceUpdateMsg).Methods("PUT")
	iot.HandleFunc("/{sn}/parameters", a.deviceGetSupportedParametersMsg).Methods("PUT")
	iot.HandleFunc("/{sn}/instances", a.deviceGetParameterInstances).Methods("PUT")
	iot.HandleFunc("/{sn}/update", a.deviceFwUpdate).Methods("PUT")
	iot.HandleFunc("/{sn}/wifi", a.deviceWifi).Methods("PUT", "GET")

	// Middleware for requests which requires user to be authenticated
	iot.Use(func(handler http.Handler) http.Handler {
		return middleware.Middleware(handler)
	})

	users := r.PathPrefix("/api/users").Subrouter()
	users.HandleFunc("", a.retrieveUsers).Methods("GET")

	users.Use(func(handler http.Handler) http.Handler {
		return middleware.Middleware(handler)
	})

	// Verifies CORS configs for requests
	corsOpts := cors.GetCorsConfig()

	srv := &http.Server{
		Addr: "0.0.0.0:" + a.Port,
		// Good practice to set timeouts to avoid Slowloris attacks.
		WriteTimeout: time.Second * 60,
		ReadTimeout:  time.Second * 60,
		IdleTimeout:  time.Second * 60,
		Handler:      corsOpts.Handler(r), // Pass our instance of gorilla/mux in.
	}

	// Run our server in a goroutine so that it doesn't block.
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Println(err)
		}
	}()
	log.Println("Running Api at port", a.Port)
}

func (a *Api) retrieveDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := a.Db.RetrieveDevices()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	err = json.NewEncoder(w).Encode(devices)
	if err != nil {
		log.Println(err)
	}

	return
}

func (a *Api) retrieveUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.Db.FindAllUsers()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	for _, x := range users {
		delete(x, "password")
	}

	err = json.NewEncoder(w).Encode(users)
	if err != nil {
		log.Println(err)
	}
	return
}

// Check which fw image is activated
func checkAvaiableFwPartition(reqPathResult []*usp_msg.GetResp_RequestedPathResult) int {
	for _, x := range reqPathResult {
		partitionsNumber := len(x.ResolvedPathResults)
		if partitionsNumber > 1 {
			log.Printf("Device has %d firmware partitions", partitionsNumber)
		}
		for i, y := range x.ResolvedPathResults {
			if y.ResultParams["Status"] == "Available" {
				log.Printf("Partition %d is avaiable", i)
				return i
			}
		}
	}
	return -1
}

func (a *Api) deviceFwUpdate(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sn := vars["sn"]
	a.deviceExists(sn, w)

	msg := utils.NewGetMsg(usp_msg.Get{
		ParamPaths: []string{"Device.DeviceInfo.FirmwareImage.*.Status"},
		MaxDepth:   1,
	})
	encodedMsg, err := proto.Marshal(&msg)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	record := utils.NewUspRecord(encodedMsg, sn)
	tr369Message, err := proto.Marshal(&record)
	if err != nil {
		log.Fatalln("Failed to encode tr369 record:", err)
	}

	a.MsgQueue[msg.Header.MsgId] = make(chan usp_msg.Msg)
	log.Println("Sending Msg:", msg.Header.MsgId)
	a.Broker.Publish(tr369Message, "oktopus/v1/agent/"+sn, "oktopus/v1/api/"+sn, false)

	var getMsgAnswer *usp_msg.GetResp

	select {
	case msg := <-a.MsgQueue[msg.Header.MsgId]:
		log.Printf("Received Msg: %s", msg.Header.MsgId)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		getMsgAnswer = msg.Body.GetResponse().GetGetResp()
	case <-time.After(time.Second * 55):
		log.Printf("Request %s Timed Out", msg.Header.MsgId)
		w.WriteHeader(http.StatusGatewayTimeout)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode("Request Timed Out")
		return
	}

	// Check which fw image is activated
	partition := checkAvaiableFwPartition(getMsgAnswer.ReqPathResults)
	if partition < 0 {
		log.Println("Error to get device available firmware partition, probably it has only one partition")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode("Server don't have the hability to update device with only one partition")
		return
		//TODO: update device with only one partition
	}

	var receiver = usp_msg.Operate{
		Command:    "Device.DeviceInfo.FirmwareImage.1.Download()",
		CommandKey: "Download()",
		SendResp:   true,
		InputArgs: map[string]string{
			"URL":          "http://cronos.intelbras.com.br/download/PON/121AC/beta/121AC-2.3-230620-77753201df4f1e2c607a7236746c8491.tar", //TODO: use dynamic url
			"AutoActivate": "true",
			//"Username": "",
			//"Password": "",
			"FileSize": "0", //TODO: send firmware length
			//"CheckSumAlgorithm": "",
			//"CheckSum":          "",
		},
	}

	msg = utils.NewOperateMsg(receiver)
	encodedMsg, err = proto.Marshal(&msg)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	record = utils.NewUspRecord(encodedMsg, sn)
	tr369Message, err = proto.Marshal(&record)
	if err != nil {
		log.Fatalln("Failed to encode tr369 record:", err)
	}

	a.MsgQueue[msg.Header.MsgId] = make(chan usp_msg.Msg)
	log.Println("Sending Msg:", msg.Header.MsgId)
	a.Broker.Publish(tr369Message, "oktopus/v1/agent/"+sn, "oktopus/v1/api/"+sn, false)

	select {
	case msg := <-a.MsgQueue[msg.Header.MsgId]:
		log.Printf("Received Msg: %s", msg.Header.MsgId)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode(msg.Body.GetResponse().GetSetResp())
		return
	case <-time.After(time.Second * 55):
		log.Printf("Request %s Timed Out", msg.Header.MsgId)
		w.WriteHeader(http.StatusGatewayTimeout)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode("Request Timed Out")
		return
	}
}

func (a *Api) deviceWifi(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sn := vars["sn"]
	a.deviceExists(sn, w)

	if r.Method == http.MethodGet {
		msg := utils.NewGetMsg(usp_msg.Get{
			ParamPaths: []string{
				"Device.WiFi.SSID.[Enable==true].SSID",
				//"Device.WiFi.AccessPoint.[Enable==true].SSIDReference",
				"Device.WiFi.AccessPoint.[Enable==true].Security.ModeEnabled",
				"Device.WiFi.AccessPoint.[Enable==true].Security.ModesSupported",
				//"Device.WiFi.EndPoint.[Enable==true].",
				"Device.WiFi.Radio.[Enable==true].AutoChannelEnable",
				"Device.WiFi.Radio.[Enable==true].Channel",
				"Device.WiFi.Radio.[Enable==true].CurrentOperatingChannelBandwidth",
				"Device.WiFi.Radio.[Enable==true].OperatingFrequencyBand",
				//"Device.WiFi.Radio.[Enable==true].PossibleChannels",
				"Device.WiFi.Radio.[Enable==true].SupportedOperatingChannelBandwidths",
			},
			MaxDepth: 2,
		})

		encodedMsg, err := proto.Marshal(&msg)
		if err != nil {
			log.Println(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		record := utils.NewUspRecord(encodedMsg, sn)
		tr369Message, err := proto.Marshal(&record)
		if err != nil {
			log.Fatalln("Failed to encode tr369 record:", err)
		}

		//a.Broker.Request(tr369Message, usp_msg.Header_GET, "oktopus/v1/agent/"+sn, "oktopus/v1/get/"+sn)
		a.MsgQueue[msg.Header.MsgId] = make(chan usp_msg.Msg)
		log.Println("Sending Msg:", msg.Header.MsgId)
		a.Broker.Publish(tr369Message, "oktopus/v1/agent/"+sn, "oktopus/v1/api/"+sn, false)

		//TODO: verify in protocol and in other models, the Device.Wifi parameters. Maybe in the future, to use SSIDReference from AccessPoint
		select {
		case msg := <-a.MsgQueue[msg.Header.MsgId]:
			log.Printf("Received Msg: %s", msg.Header.MsgId)
			delete(a.MsgQueue, msg.Header.MsgId)
			log.Println("requests queue:", a.MsgQueue)
			answer := msg.Body.GetResponse().GetGetResp()

			var wifi [2]WiFi

			//TODO: better algorithm, might use something faster an more reliable
			//TODO: full fill the commented wifi resources
			for _, x := range answer.ReqPathResults {
				if x.RequestedPath == "Device.WiFi.SSID.[Enable==true].SSID" {
					for i, y := range x.ResolvedPathResults {
						wifi[i].SSID = y.ResultParams["SSID"]
					}
					continue
				}
				if x.RequestedPath == "Device.WiFi.AccessPoint.[Enable==true].Security.ModeEnabled" {
					for i, y := range x.ResolvedPathResults {
						wifi[i].Security = y.ResultParams["Security.ModeEnabled"]
					}
					continue
				}
				if x.RequestedPath == "Device.WiFi.AccessPoint.[Enable==true].Security.ModesSupported" {
					for i, y := range x.ResolvedPathResults {
						wifi[i].SecurityCapabilities = strings.Split(y.ResultParams["Security.ModesSupported"], ",")
					}
					continue
				}
				if x.RequestedPath == "Device.WiFi.Radio.[Enable==true].AutoChannelEnable" {
					for i, y := range x.ResolvedPathResults {
						autoChannel, err := strconv.ParseBool(y.ResultParams["AutoChannelEnable"])
						if err != nil {
							log.Println(err)
							wifi[i].AutoChannelEnable = false
						} else {
							wifi[i].AutoChannelEnable = autoChannel
						}
					}
					continue
				}
				if x.RequestedPath == "Device.WiFi.Radio.[Enable==true].Channel" {
					for i, y := range x.ResolvedPathResults {
						channel, err := strconv.Atoi(y.ResultParams["Channel"])
						if err != nil {
							log.Println(err)
							wifi[i].Channel = -1
						} else {
							wifi[i].Channel = channel
						}
					}
					continue
				}
				if x.RequestedPath == "Device.WiFi.Radio.[Enable==true].CurrentOperatingChannelBandwidth" {
					for i, y := range x.ResolvedPathResults {
						wifi[i].ChannelBandwidth = y.ResultParams["CurrentOperatingChannelBandwidth"]
					}
					continue
				}
				if x.RequestedPath == "Device.WiFi.Radio.[Enable==true].OperatingFrequencyBand" {
					for i, y := range x.ResolvedPathResults {
						wifi[i].FrequencyBand = y.ResultParams["OperatingFrequencyBand"]
					}
					continue
				}
				if x.RequestedPath == "Device.WiFi.Radio.[Enable==true].SupportedOperatingChannelBandwidths" {
					for i, y := range x.ResolvedPathResults {
						wifi[i].SupportedChannelBandwidths = strings.Split(y.ResultParams["SupportedOperatingChannelBandwidths"], ",")
					}
					continue
				}
			}
			json.NewEncoder(w).Encode(&wifi)
			return
		case <-time.After(time.Second * 45):
			log.Printf("Request %s Timed Out", msg.Header.MsgId)
			w.WriteHeader(http.StatusGatewayTimeout)
			delete(a.MsgQueue, msg.Header.MsgId)
			log.Println("requests queue:", a.MsgQueue)
			json.NewEncoder(w).Encode("Request Timed Out")
			return
		}
	}
}

func (a *Api) deviceGetParameterInstances(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sn := vars["sn"]
	a.deviceExists(sn, w)

	var receiver usp_msg.GetInstances

	err := json.NewDecoder(r.Body).Decode(&receiver)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msg := utils.NewGetParametersInstancesMsg(receiver)
	encodedMsg, err := proto.Marshal(&msg)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	record := utils.NewUspRecord(encodedMsg, sn)
	tr369Message, err := proto.Marshal(&record)
	if err != nil {
		log.Fatalln("Failed to encode tr369 record:", err)
	}

	//a.Broker.Request(tr369Message, usp_msg.Header_GET, "oktopus/v1/agent/"+sn, "oktopus/v1/get/"+sn)
	a.MsgQueue[msg.Header.MsgId] = make(chan usp_msg.Msg)
	log.Println("Sending Msg:", msg.Header.MsgId)
	a.Broker.Publish(tr369Message, "oktopus/v1/agent/"+sn, "oktopus/v1/api/"+sn, false)

	select {
	case msg := <-a.MsgQueue[msg.Header.MsgId]:
		log.Printf("Received Msg: %s", msg.Header.MsgId)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode(msg.Body.GetResponse().GetGetInstancesResp())
		return
	case <-time.After(time.Second * 55):
		log.Printf("Request %s Timed Out", msg.Header.MsgId)
		w.WriteHeader(http.StatusGatewayTimeout)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode("Request Timed Out")
		return
	}
}

func (a *Api) deviceGetSupportedParametersMsg(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sn := vars["sn"]
	a.deviceExists(sn, w)

	var receiver usp_msg.GetSupportedDM

	err := json.NewDecoder(r.Body).Decode(&receiver)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msg := utils.NewGetSupportedParametersMsg(receiver)
	encodedMsg, err := proto.Marshal(&msg)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	record := utils.NewUspRecord(encodedMsg, sn)
	tr369Message, err := proto.Marshal(&record)
	if err != nil {
		log.Fatalln("Failed to encode tr369 record:", err)
	}

	//a.Broker.Request(tr369Message, usp_msg.Header_GET, "oktopus/v1/agent/"+sn, "oktopus/v1/get/"+sn)
	a.MsgQueue[msg.Header.MsgId] = make(chan usp_msg.Msg)
	log.Println("Sending Msg:", msg.Header.MsgId)
	a.Broker.Publish(tr369Message, "oktopus/v1/agent/"+sn, "oktopus/v1/api/"+sn, false)

	select {
	case msg := <-a.MsgQueue[msg.Header.MsgId]:
		log.Printf("Received Msg: %s", msg.Header.MsgId)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode(msg.Body.GetResponse().GetGetSupportedDmResp())
		return
	case <-time.After(time.Second * 55):
		log.Printf("Request %s Timed Out", msg.Header.MsgId)
		w.WriteHeader(http.StatusGatewayTimeout)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode("Request Timed Out")
		return
	}
}

func (a *Api) deviceCreateMsg(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sn := vars["sn"]
	a.deviceExists(sn, w)

	var receiver usp_msg.Add

	err := json.NewDecoder(r.Body).Decode(&receiver)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msg := utils.NewCreateMsg(receiver)
	encodedMsg, err := proto.Marshal(&msg)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	record := utils.NewUspRecord(encodedMsg, sn)
	tr369Message, err := proto.Marshal(&record)
	if err != nil {
		log.Fatalln("Failed to encode tr369 record:", err)
	}

	//a.Broker.Request(tr369Message, usp_msg.Header_GET, "oktopus/v1/agent/"+sn, "oktopus/v1/get/"+sn)
	a.MsgQueue[msg.Header.MsgId] = make(chan usp_msg.Msg)
	log.Println("Sending Msg:", msg.Header.MsgId)
	a.Broker.Publish(tr369Message, "oktopus/v1/agent/"+sn, "oktopus/v1/api/"+sn, false)

	select {
	case msg := <-a.MsgQueue[msg.Header.MsgId]:
		log.Printf("Received Msg: %s", msg.Header.MsgId)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode(msg.Body.GetResponse().GetAddResp())
		return
	case <-time.After(time.Second * 55):
		log.Printf("Request %s Timed Out", msg.Header.MsgId)
		w.WriteHeader(http.StatusGatewayTimeout)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode("Request Timed Out")
		return
	}
}

func (a *Api) deviceGetMsg(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sn := vars["sn"]

	a.deviceExists(sn, w)

	var receiver usp_msg.Get

	err := json.NewDecoder(r.Body).Decode(&receiver)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msg := utils.NewGetMsg(receiver)
	encodedMsg, err := proto.Marshal(&msg)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	record := utils.NewUspRecord(encodedMsg, sn)
	tr369Message, err := proto.Marshal(&record)
	if err != nil {
		log.Fatalln("Failed to encode tr369 record:", err)
	}

	a.MsgQueue[msg.Header.MsgId] = make(chan usp_msg.Msg)

	log.Println("Sending Msg:", msg.Header.MsgId)
	a.Broker.Publish(tr369Message, "oktopus/v1/agent/"+sn, "oktopus/v1/api/"+sn, false)

	select {
	case msg := <-a.MsgQueue[msg.Header.MsgId]:
		log.Printf("Received Msg: %s", msg.Header.MsgId)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode(msg.Body.GetResponse().GetGetResp())
		return
	case <-time.After(time.Second * 55):
		log.Printf("Request %s Timed Out", msg.Header.MsgId)
		w.WriteHeader(http.StatusGatewayTimeout)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode("Request Timed Out")
		return
	}
}

func (a *Api) deviceDeleteMsg(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sn := vars["sn"]
	a.deviceExists(sn, w)

	var receiver usp_msg.Delete

	err := json.NewDecoder(r.Body).Decode(&receiver)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msg := utils.NewDelMsg(receiver)
	encodedMsg, err := proto.Marshal(&msg)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	record := utils.NewUspRecord(encodedMsg, sn)
	tr369Message, err := proto.Marshal(&record)
	if err != nil {
		log.Fatalln("Failed to encode tr369 record:", err)
	}

	//a.Broker.Request(tr369Message, usp_msg.Header_GET, "oktopus/v1/agent/"+sn, "oktopus/v1/get/"+sn)
	a.MsgQueue[msg.Header.MsgId] = make(chan usp_msg.Msg)
	log.Println("Sending Msg:", msg.Header.MsgId)
	a.Broker.Publish(tr369Message, "oktopus/v1/agent/"+sn, "oktopus/v1/api/"+sn, false)

	select {
	case msg := <-a.MsgQueue[msg.Header.MsgId]:
		log.Printf("Received Msg: %s", msg.Header.MsgId)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode(msg.Body.GetResponse().GetDeleteResp())
		return
	case <-time.After(time.Second * 55):
		log.Printf("Request %s Timed Out", msg.Header.MsgId)
		w.WriteHeader(http.StatusGatewayTimeout)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode("Request Timed Out")
		return
	}
}

func (a *Api) deviceUpdateMsg(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sn := vars["sn"]
	a.deviceExists(sn, w)

	var receiver usp_msg.Set

	err := json.NewDecoder(r.Body).Decode(&receiver)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msg := utils.NewSetMsg(receiver)
	encodedMsg, err := proto.Marshal(&msg)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	record := utils.NewUspRecord(encodedMsg, sn)
	tr369Message, err := proto.Marshal(&record)
	if err != nil {
		log.Fatalln("Failed to encode tr369 record:", err)
	}

	//a.Broker.Request(tr369Message, usp_msg.Header_GET, "oktopus/v1/agent/"+sn, "oktopus/v1/get/"+sn)
	a.MsgQueue[msg.Header.MsgId] = make(chan usp_msg.Msg)
	log.Println("Sending Msg:", msg.Header.MsgId)
	a.Broker.Publish(tr369Message, "oktopus/v1/agent/"+sn, "oktopus/v1/api/"+sn, false)

	select {
	case msg := <-a.MsgQueue[msg.Header.MsgId]:
		log.Printf("Received Msg: %s", msg.Header.MsgId)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode(msg.Body.GetResponse().GetSetResp())
		return
	case <-time.After(time.Second * 55):
		log.Printf("Request %s Timed Out", msg.Header.MsgId)
		w.WriteHeader(http.StatusGatewayTimeout)
		delete(a.MsgQueue, msg.Header.MsgId)
		log.Println("requests queue:", a.MsgQueue)
		json.NewEncoder(w).Encode("Request Timed Out")
		return
	}
}

func (a *Api) deviceExists(sn string, w http.ResponseWriter) {
	_, err := a.Db.RetrieveDevice(sn)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode("No device with serial number " + sn + " was found")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (a *Api) registerUser(w http.ResponseWriter, r *http.Request) {

	tokenString := r.Header.Get("Authorization")
	if tokenString == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	email, err := auth.ValidateToken(tokenString)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	//Check if user which is requesting creation has the necessary privileges
	rUser, err := a.Db.FindUser(email)
	if rUser.Level != AdminUser {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	var user db.User
	err = json.NewDecoder(r.Body).Decode(&user)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	user.Level = NormalUser

	if err := user.HashPassword(user.Password); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := a.Db.RegisterUser(user); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (a *Api) registerAdminUser(w http.ResponseWriter, r *http.Request) {

	var user db.User
	err := json.NewDecoder(r.Body).Decode(&user)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	users, err := a.Db.FindAllUsers()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	adminExists := adminUserExists(users)
	if adminExists {
		log.Println("There might exist only one admin")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode("There might exist only one admin")
		return
	}

	user.Level = AdminUser

	if err := user.HashPassword(user.Password); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := a.Db.RegisterUser(user); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func adminUserExists(users []map[string]interface{}) bool {
	for _, x := range users {
		if x["level"].(int32) == AdminUser {
			log.Println("Admin exists")
			return true
		}
	}
	return false
}

func (a *Api) adminUserExists(w http.ResponseWriter, r *http.Request) {

	users, err := a.Db.FindAllUsers()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	adminExits := adminUserExists(users)
	json.NewEncoder(w).Encode(adminExits)
	return
}

type TokenRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (a *Api) generateToken(w http.ResponseWriter, r *http.Request) {
	var tokenReq TokenRequest

	err := json.NewDecoder(r.Body).Decode(&tokenReq)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	user, err := a.Db.FindUser(tokenReq.Email)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode("Invalid Credentials")
		return
	}

	credentialError := user.CheckPassword(tokenReq.Password)
	if credentialError != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode("Invalid Credentials")
		return
	}

	token, err := auth.GenerateJWT(user.Email, user.Name)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(token)
	return
}
