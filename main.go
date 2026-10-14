package main

import (
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/skip2/go-qrcode"
)

const (
	GuildID  = "1302203140606791680"
	WebURL   = "https://csign.darkyu.me"
	Port     = ":8080"
	DataFile = "data.json"
)

type Session struct {
	Code      string            `json:"code"`
	Token     string            `json:"token"`
	StartTime time.Time         `json:"start_time"`
	EndTime   time.Time         `json:"end_time"`
	Attendees map[string]string `json:"attendees"`
}

type AppData struct {
	MemberList []string           `json:"member_list"`
	Sessions   map[string]Session `json:"-"`
}

var (
	store = AppData{
		MemberList: []string{},
		Sessions:   make(map[string]Session),
	}
	mu      sync.Mutex
	codeReg = regexp.MustCompile(`^[a-zA-Z0-9]{4}$`)
	twTz, _ = time.LoadLocation("Asia/Taipei")
)

func generateRandomToken() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func loadData() {
	mu.Lock()
	defer mu.Unlock()
	file, err := os.ReadFile(DataFile)
	if err == nil {
		_ = json.Unmarshal(file, &store)
	}
	store.Sessions = make(map[string]Session)
	saveDataLocked()
}

func saveDataLocked() {
	data, err := json.MarshalIndent(store, "", "  ")
	if err == nil {
		_ = os.WriteFile(DataFile, data, 0644)
	}
}

func main() {
	botToken := os.Getenv("DISCORD_TOKEN")
	if botToken == "" {
		log.Fatal("錯誤：未設定 DISCORD_TOKEN 環境變數")
	}

	if twTz == nil {
		twTz = time.FixedZone("CST", 8*3600)
	}
	loadData()

	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Fatalf("建立 Discord session 失敗: %v", err)
	}

	dg.AddHandler(onInteractionCreate)

	if err := dg.Open(); err != nil {
		log.Fatalf("無法連線至 Discord: %v", err)
	}
	defer dg.Close()

	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "create",
			Description: "建立開會簽到 QR Code",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "time",
					Description: "開放進入時間 (格式: 19:00 或 2026-09-30 19:00)",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "durations",
					Description: "有效時間 (單位: 分鐘，例如 15)",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "code",
					Description: "4位英數識別碼 (例如 A1B2)",
					Required:    true,
				},
			},
		},
		{
			Name:        "delete",
			Description: "刪除指定的開會簽到活動",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "code",
					Description: "要刪除的4位英數識別碼",
					Required:    true,
				},
			},
		},
		{
			Name:        "enterlist",
			Description: "設定應參與開會的社員名單 (會永久記憶，覆蓋舊名單)",
		},
		{
			Name:        "check",
			Description: "根據識別碼確認出席與缺席名單",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "code",
					Description: "4位英數識別碼",
					Required:    true,
				},
			},
		},
		{
			Name:        "export",
			Description: "將簽到紀錄匯出成 Excel (CSV) 檔案",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "code",
					Description: "選填：指定要匯出的4位識別碼（若不填則匯出本次所有場次）",
					Required:    false,
				},
			},
		},
	}

	for _, cmd := range commands {
		_, err := dg.ApplicationCommandCreate(dg.State.User.ID, GuildID, cmd)
		if err != nil {
			log.Fatalf("註冊指令 %s 失敗: %v", cmd.Name, err)
		}
	}
	log.Println("Discord Bot 已上線，指令註冊完成！")

	http.HandleFunc("/api/status", corsMiddleware(handleStatus))
	http.HandleFunc("/api/signin", corsMiddleware(handleSignIn))

	go func() {
		log.Printf("API 伺服器啟動於 %s\n", Port)
		if err := http.ListenAndServe(Port, nil); err != nil {
			log.Fatalf("HTTP Server 錯誤: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	mu.Lock()
	store.Sessions = make(map[string]Session)
	saveDataLocked()
	mu.Unlock()
	log.Println("已清除本次所有簽到紀錄（保留應到社員名單），正在關閉機器人...")
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	queryVal := strings.TrimSpace(r.URL.Query().Get("s"))
	if queryVal == "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"blocked": false})
		return
	}

	now := time.Now().In(twTz)
	mu.Lock()
	var targetSess Session
	var exists bool

	for _, sess := range store.Sessions {
		if strings.EqualFold(sess.Token, queryVal) || strings.EqualFold(sess.Code, queryVal) {
			targetSess = sess
			exists = true
			break
		}
	}
	mu.Unlock()

	if !exists {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"blocked": true,
			"reason":  "❌ 無效或已刪除的簽到 QR Code",
		})
		return
	}
	if now.Before(targetSess.StartTime) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"blocked": true,
			"reason":  fmt.Sprintf("⏳ 簽到尚未開始！\n開放時間：%s", targetSess.StartTime.Format("2006/01/02 15:04")),
		})
		return
	}
	if now.After(targetSess.EndTime) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"blocked": true,
			"reason":  "⌛ 此 QR Code 已過期失效！",
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{"blocked": false})
}

func handleSignIn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "請求格式錯誤"})
		return
	}

	name := strings.TrimSpace(req.Name)
	code := strings.ToUpper(strings.TrimSpace(req.Code))

	if name == "" || code == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "請填寫姓名與識別碼"})
		return
	}

	now := time.Now().In(twTz)

	mu.Lock()
	defer mu.Unlock()

	sess, exists := store.Sessions[code]
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "識別碼錯誤，找不到此簽到場次"})
		return
	}

	if now.Before(sess.StartTime) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": fmt.Sprintf("簽到尚未開始（開放時間：%s）", sess.StartTime.Format("15:04")),
		})
		return
	}

	if now.After(sess.EndTime) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "超過簽到時間，此識別碼已失效"})
		return
	}

	if sess.Attendees == nil {
		sess.Attendees = make(map[string]string)
	}
	sess.Attendees[name] = now.Format("2006/01/02 15:04:05")
	store.Sessions[code] = sess

	_ = json.NewEncoder(w).Encode(map[string]string{
		"message": fmt.Sprintf("%s 簽到成功！", name),
	})
}

func onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()
		switch data.Name {
		case "create":
			handleCreateCmd(s, i, data)
		case "delete":
			handleDeleteCmd(s, i, data)
		case "enterlist":
			handleEnterListCmd(s, i)
		case "check":
			handleCheckCmd(s, i, data)
		case "export":
			handleExportCmd(s, i, data)
		}

	case discordgo.InteractionModalSubmit:
		data := i.ModalSubmitData()
		if data.CustomID == "modal_enterlist" {
			rawList := data.Components[0].(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput).Value
			replacer := strings.NewReplacer(",", "\n", "，", "\n", "、", "\n", " ", "\n", "\r", "")
			lines := strings.Split(replacer.Replace(rawList), "\n")

			var newList []string
			seen := make(map[string]bool)
			for _, line := range lines {
				name := strings.TrimSpace(line)
				if name != "" && !seen[name] {
					seen[name] = true
					newList = append(newList, name)
				}
			}

			mu.Lock()
			store.MemberList = newList
			saveDataLocked()
			mu.Unlock()

			msg := fmt.Sprintf("✅ 已成功更新應到名單（共 **%d** 人）：\n`%s`", len(newList), strings.Join(newList, "、"))
			respondText(s, i, msg)
		}
	}
}

func handleCreateCmd(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	var timeStr, code string
	var durations int64

	for _, opt := range data.Options {
		switch opt.Name {
		case "time":
			timeStr = strings.TrimSpace(opt.StringValue())
		case "durations":
			durations = opt.IntValue()
		case "code":
			code = strings.ToUpper(strings.TrimSpace(opt.StringValue()))
		}
	}

	if !codeReg.MatchString(code) {
		respondText(s, i, "❌ 識別碼 `code` 必須剛好是 **4 個英文字母或數字**！")
		return
	}
	if durations <= 0 {
		respondText(s, i, "❌ 有效時間 `durations` 必須大於 0 分鐘！")
		return
	}

	now := time.Now().In(twTz)
	timeStr = strings.ReplaceAll(timeStr, "/", "-")
	var startTime time.Time
	var err error

	if len(timeStr) <= 5 {
		fullStr := fmt.Sprintf("%s %s", now.Format("2006-01-02"), timeStr)
		startTime, err = time.ParseInLocation("2006-01-02 15:04", fullStr, twTz)
	} else {
		startTime, err = time.ParseInLocation("2006-01-02 15:04", timeStr, twTz)
	}

	if err != nil {
		respondText(s, i, "❌ 時間格式錯誤！請輸入例如 `19:00` 或 `2026-09-30 19:00`")
		return
	}

	endTime := startTime.Add(time.Duration(durations) * time.Minute)
	token := generateRandomToken()

	mu.Lock()
	store.Sessions[code] = Session{
		Code:      code,
		Token:     token,
		StartTime: startTime,
		EndTime:   endTime,
		Attendees: make(map[string]string),
	}
	mu.Unlock()

	qrURL := fmt.Sprintf("%s/?s=%s", WebURL, token)
	pngBytes, err := qrcode.Encode(qrURL, qrcode.Medium, 300)
	if err != nil {
		respondText(s, i, "❌ 產生 QR Code 失敗")
		return
	}

	content := fmt.Sprintf(
		"**開會簽到已建立！**\n"+
			"識別碼：`%s`\n"+
			"開放時間：`%s` ～ `%s`（共 %d 分鐘）\n"+
			"簽到網址：%s",
		code,
		startTime.Format("2006/01/02 15:04"),
		endTime.Format("15:04"),
		durations,
		WebURL,
	)

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Files: []*discordgo.File{
				{
					Name:   fmt.Sprintf("signin_%s.png", code),
					Reader: bytes.NewReader(pngBytes),
				},
			},
		},
	})
}

// /delete 指令邏輯：刪除指定 code 的簽到活動
func handleDeleteCmd(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	code := strings.ToUpper(strings.TrimSpace(data.Options[0].StringValue()))

	mu.Lock()
	_, exists := store.Sessions[code]
	if exists {
		delete(store.Sessions, code)
	}
	mu.Unlock()

	if !exists {
		respondText(s, i, fmt.Sprintf("❌ 找不到識別碼為 `%s` 的簽到活動，無法刪除！", code))
		return
	}

	respondText(s, i, fmt.Sprintf("🗑️ 已成功刪除識別碼為 `%s` 的簽到活動！", code))
}

func handleEnterListCmd(s *discordgo.Session, i *discordgo.InteractionCreate) {
	mu.Lock()
	currentList := strings.Join(store.MemberList, "\n")
	mu.Unlock()

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: "modal_enterlist",
			Title:    "設定應到社員名單",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							CustomID:    "input_names",
							Label:       "請輸入姓名（一行一個，或以空白/逗號分隔）",
							Style:       discordgo.TextInputParagraph,
							Placeholder: "王小明\n陳大華\n林小美",
							Value:       currentList,
							Required:    true,
						},
					},
				},
			},
		},
	})
}

func handleCheckCmd(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	code := strings.ToUpper(strings.TrimSpace(data.Options[0].StringValue()))

	mu.Lock()
	session, exists := store.Sessions[code]
	members := make([]string, len(store.MemberList))
	copy(members, store.MemberList)
	mu.Unlock()

	if !exists {
		respondText(s, i, fmt.Sprintf("❌ 找不到識別碼為 `%s` 的簽到場次！", code))
		return
	}

	if len(members) == 0 {
		respondText(s, i, "⚠️ 尚未設定應到名單！請先使用 `/enterlist` 設定名單。")
		return
	}

	var present []string
	var absent []string
	memberSet := make(map[string]bool)

	for _, name := range members {
		memberSet[name] = true
		if _, ok := session.Attendees[name]; ok {
			present = append(present, name)
		} else {
			absent = append(absent, name)
		}
	}

	var extra []string
	for signedName := range session.Attendees {
		if !memberSet[signedName] {
			extra = append(extra, signedName)
		}
	}

	presentStr := "無"
	if len(present) > 0 {
		presentStr = strings.Join(present, "、")
	}
	absentStr := "無"
	if len(absent) > 0 {
		absentStr = strings.Join(absent, "、")
	}

	reply := fmt.Sprintf(
		"**簽到狀況查詢（識別碼：`%s`）**\n"+
			"場次時間：`%s` - `%s`\n\n"+
			"✅ **已到 (%d/%d)：**\n%s\n\n"+
			"❌ **未到 (%d/%d)：**\n%s",
		code,
		session.StartTime.Format("2006/01/02 15:04"),
		session.EndTime.Format("15:04"),
		len(present), len(members), presentStr,
		len(absent), len(members), absentStr,
	)

	if len(extra) > 0 {
		reply += fmt.Sprintf("\n\n❓ **名單外簽到 (%d)：**\n%s", len(extra), strings.Join(extra, "、"))
	}

	respondText(s, i, reply)
}

func handleExportCmd(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	var targetCode string
	if len(data.Options) > 0 {
		targetCode = strings.ToUpper(strings.TrimSpace(data.Options[0].StringValue()))
	}

	mu.Lock()
	var sessionsToExport []Session
	if targetCode != "" {
		if sess, exists := store.Sessions[targetCode]; exists {
			sessionsToExport = append(sessionsToExport, sess)
		} else {
			mu.Unlock()
			respondText(s, i, fmt.Sprintf("❌ 找不到識別碼為 `%s` 的簽到場次！", targetCode))
			return
		}
	} else {
		for _, sess := range store.Sessions {
			sessionsToExport = append(sessionsToExport, sess)
		}
	}
	mu.Unlock()

	if len(sessionsToExport) == 0 {
		respondText(s, i, "⚠️ 目前沒有任何簽到場次可供匯出！")
		return
	}

	var buf bytes.Buffer
	buf.Write([]byte{0xEF, 0xBB, 0xBF})

	writer := csv.NewWriter(&buf)
	_ = writer.Write([]string{"名稱", "簽到時間", "識別碼"})

	type rowRecord struct {
		name     string
		signTime string
		code     string
	}
	var rows []rowRecord

	for _, sess := range sessionsToExport {
		for name, signTime := range sess.Attendees {
			rows = append(rows, rowRecord{
				name:     name,
				signTime: signTime,
				code:     sess.Code,
			})
		}
	}

	if len(rows) == 0 {
		respondText(s, i, "⚠️ 該場次目前還沒有任何人完成簽到喔！")
		return
	}

	sort.Slice(rows, func(a, b int) bool {
		return rows[a].signTime < rows[b].signTime
	})

	for _, r := range rows {
		_ = writer.Write([]string{r.name, r.signTime, r.code})
	}
	writer.Flush()

	fileName := fmt.Sprintf("signin_export_%s.csv", time.Now().In(twTz).Format("20060102_1504"))
	if targetCode != "" {
		fileName = fmt.Sprintf("signin_%s_%s.csv", targetCode, time.Now().In(twTz).Format("20060102"))
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("📥 **簽到紀錄匯出完成！**（共 **%d** 筆簽到資料，可直接用 Excel 開啟）", len(rows)),
			Files: []*discordgo.File{
				{
					Name:   fileName,
					Reader: bytes.NewReader(buf.Bytes()),
				},
			},
		},
	})
}

func respondText(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
		},
	})
}
