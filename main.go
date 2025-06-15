package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	Host           = "https://openapi.tuyaeu.com"
	ClientID       = "hskykhepdrxhmygcg5cd"
	Secret         = "c28b99733f7c4c6cb4fe3b3f7d8e283f"
	UpdateInterval = 2 * time.Second
	TokenExpiry    = 4 * time.Hour
)

var (
	Token          string
	tokenExpiry    time.Time
	lastEnergyData EnergyData
	dataMutex      sync.RWMutex
	clients        map[chan string]bool
	clientsMutex   sync.Mutex
)

type DeviceStatus struct {
	Code  string      `json:"code"`
	Value interface{} `json:"value"`
}

type DeviceResponse struct {
	Result  []DeviceStatus `json:"result"`
	Success bool           `json:"success"`
	T       int64          `json:"t"`
}

type EnergyData struct {
	PainelSolar    float64 `json:"painel_solar"`
	ImportacaoRede float64 `json:"importacao_rede"`
	CarregadorEV   float64 `json:"carregador_ev"`
	ConsumoCasa    float64 `json:"consumo_casa"`
	VoltagemMedia  float64 `json:"voltagem_media"`
	LastUpdate     string  `json:"last_update"`
	Timestamp      int64   `json:"timestamp"`
}

var devices = map[string]map[string]string{
	"bfe2a8f17ebd1d39d1hctm": {
		"cur_power2":   "importacao_rede",
		"cur_voltage2": "voltagem",
	},
	"bfcaa7d812363ec7eeimjs": {
		"cur_power1":   "carregador_ev",
		"cur_power2":   "painel_solar",
		"cur_voltage1": "voltagem",
	},
}

func main() {
	clients = make(map[chan string]bool)

	if err := refreshToken(); err != nil {
		log.Fatal("Initial token refresh failed:", err)
	}
	go dataUpdater()
	go tokenRefresher()

	http.HandleFunc("/", homeHandler)
	http.HandleFunc("/api/energy", energyHandler)
	http.HandleFunc("/api/stream", sseHandler)
	http.HandleFunc("/api/refresh", refreshHandler)
	http.HandleFunc("/static/", staticHandler)

	log.Println("Server running on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/index.html")
}

func staticHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, r.URL.Path[1:])
}

func energyHandler(w http.ResponseWriter, r *http.Request) {
	dataMutex.RLock()
	data := lastEnergyData
	dataMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func refreshHandler(w http.ResponseWriter, r *http.Request) {
	newData, err := fetchEnergyData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	dataMutex.Lock()
	lastEnergyData = *newData
	dataMutex.Unlock()

	broadcastUpdate(newData)
	w.WriteHeader(http.StatusOK)
}

func dataUpdater() {
	ticker := time.NewTicker(UpdateInterval)
	defer ticker.Stop()

	for range ticker.C {
		newData, err := fetchEnergyData()
		if err != nil {
			log.Printf("Data update error: %v", err)
			continue
		}

		dataMutex.Lock()
		lastEnergyData = *newData
		dataMutex.Unlock()

		broadcastUpdate(newData)
	}
}

func fetchEnergyData() (*EnergyData, error) {
	data := &EnergyData{
		LastUpdate: time.Now().Format("2006-01-02 15:04:05"),
		Timestamp:  time.Now().Unix(),
	}

	var voltageSum float64
	voltageCount := 0

	for deviceID, mappings := range devices {
		status, err := getDeviceStatus(deviceID)
		if err != nil {
			return nil, fmt.Errorf("device %s error: %v", deviceID, err)
		}

		for _, item := range status.Result {
			if mapping, exists := mappings[item.Code]; exists {
				value := convertToFloat(item.Value) * 0.1

				switch mapping {
				case "painel_solar":
					data.PainelSolar = value
				case "importacao_rede":
					data.ImportacaoRede = value
				case "carregador_ev":
					data.CarregadorEV = value
				case "voltagem":
					voltageSum += value
					voltageCount++
				}
			}
		}
	}

	if voltageCount > 0 {
		data.VoltagemMedia = voltageSum / float64(voltageCount)
	}

	data.ConsumoCasa = data.PainelSolar + data.ImportacaoRede - data.CarregadorEV
	return data, nil
}

func getDeviceStatus(deviceID string) (*DeviceResponse, error) {
	method := "GET"
	req, err := http.NewRequest(method, Host+"/v1.0/devices/"+deviceID+"/status", nil)
	if err != nil {
		return nil, err
	}

	buildHeader(req, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var deviceResp DeviceResponse
	if err := json.NewDecoder(resp.Body).Decode(&deviceResp); err != nil {
		return nil, err
	}

	return &deviceResp, nil
}

func refreshToken() error {
	method := "GET"
	req, err := http.NewRequest(method, Host+"/v1.0/token?grant_type=1", nil)
	if err != nil {
		return err
	}

	buildHeader(req, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var tokenResp struct {
		Result struct {
			AccessToken string `json:"access_token"`
			ExpireTime  int    `json:"expire_time"`
		} `json:"result"`
		Success bool `json:"success"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return err
	}

	if !tokenResp.Success {
		return fmt.Errorf("failed to get token")
	}

	Token = tokenResp.Result.AccessToken
	tokenExpiry = time.Now().Add(time.Duration(tokenResp.Result.ExpireTime) * time.Second)
	log.Println("Token refreshed successfully")
	return nil
}

func tokenRefresher() {
	ticker := time.NewTicker(TokenExpiry - 30*time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		if time.Now().After(tokenExpiry.Add(-30 * time.Minute)) {
			if err := refreshToken(); err != nil {
				log.Printf("Token refresh error: %v", err)
			}
		}
	}
}

func buildHeader(req *http.Request, body []byte) {
	req.Header.Set("client_id", ClientID)
	req.Header.Set("sign_method", "HMAC-SHA256")
	req.Header.Set("t", fmt.Sprint(time.Now().UnixNano()/1e6))

	if Token != "" {
		req.Header.Set("access_token", Token)
	}

	sign := buildSign(req, body)
	req.Header.Set("sign", sign)
}

func buildSign(req *http.Request, body []byte) string {
	headers := getHeaderStr(req)
	urlStr := getUrlStr(req)
	contentSha256 := sha256Hash(body)
	stringToSign := req.Method + "\n" + contentSha256 + "\n" + headers + "\n" + urlStr
	signStr := ClientID + Token + req.Header.Get("t") + stringToSign
	return strings.ToUpper(hmacSha256(signStr, Secret))
}

func sha256Hash(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func hmacSha256(message, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func getUrlStr(req *http.Request) string {
	url := req.URL.Path
	query := req.URL.Query()

	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for i, k := range keys {
		if i == 0 {
			url += "?"
		} else {
			url += "&"
		}
		url += k + "=" + query.Get(k)
	}

	return url
}

func getHeaderStr(req *http.Request) string {
	signHeaders := req.Header.Get("Signature-Headers")
	if signHeaders == "" {
		return ""
	}

	headers := ""
	for _, h := range strings.Split(signHeaders, ":") {
		headers += h + ":" + req.Header.Get(h) + "\n"
	}
	return headers
}

func convertToFloat(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int:
		return float64(val)
	case string:
		var f float64
		fmt.Sscanf(val, "%f", &f)
		return f
	default:
		return 0
	}
}

func broadcastUpdate(data *EnergyData) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("Error marshaling update: %v", err)
		return
	}

	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	for client := range clients {
		select {
		case client <- string(jsonData):
		default:
			delete(clients, client)
			close(client)
		}
	}
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	notify := w.(http.CloseNotifier).CloseNotify()
	messageChan := make(chan string)

	clientsMutex.Lock()
	clients[messageChan] = true
	clientsMutex.Unlock()

	defer func() {
		clientsMutex.Lock()
		delete(clients, messageChan)
		close(messageChan)
		clientsMutex.Unlock()
	}()

	// Send initial data
	dataMutex.RLock()
	initialData, _ := json.Marshal(lastEnergyData)
	dataMutex.RUnlock()
	fmt.Fprintf(w, "data: %s\n\n", initialData)
	flusher.Flush()

	for {
		select {
		case msg := <-messageChan:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-notify:
			return
		case <-r.Context().Done():
			return
		}
	}
}
