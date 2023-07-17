package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mattermost/mattermost-plugin-api/experimental/telemetry"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
	"github.com/pkg/errors"
)

const (
	// WSEventRefresh is the WebSocket event for refreshing the Todo list
	WSEventRefresh = "refresh"

	// WSEventConfigUpdate is the WebSocket event to update the Todo list's configurations on webapp
	WSEventConfigUpdate = "config_update"
)

// ListManager represents the logic on the lists
type ListManager interface {
	// AddIssue adds a todo to userID's myList with the message
	AddIssue(userID, message, description, postID string) (*Issue, error)
	// SendIssue sends the todo with the message from senderID to receiverID and returns the receiver's issueID
	SendIssue(senderID, receiverID, message, description, postID string) (string, error)
	// GetIssueList gets the todos on listID for userID
	GetIssueList(userID, listID string) ([]*ExtendedIssue, error)
	// CompleteIssue completes the todo issueID for userID, and returns the issue and the foreign ID if any
	CompleteIssue(userID, issueID string) (issue *Issue, foreignID string, listToUpdate string, err error)
	// AcceptIssue moves one the todo issueID of userID from inbox to myList, and returns the message and the foreignUserID if any
	AcceptIssue(userID, issueID string) (todoMessage string, foreignUserID string, err error)
	// RemoveIssue removes the todo issueID for userID and returns the issue, the foreign ID if any and whether the user sent the todo to someone else
	RemoveIssue(userID, issueID string) (issue *Issue, foreignID string, isSender bool, listToUpdate string, err error)
	// PopIssue the first element of myList for userID and returns the issue and the foreign ID if any
	PopIssue(userID string) (issue *Issue, foreignID string, err error)
	// BumpIssue moves a issueID sent by userID to the top of its receiver inbox list
	BumpIssue(userID string, issueID string) (todoMessage string, receiver string, foreignIssueID string, err error)
	// EditIssue updates the message on an issue
	EditIssue(userID string, issueID string, newMessage string, newDescription string) (foreignUserID string, list string, oldMessage string, err error)
	// ChangeAssignment updates an issue to assign a different person
	ChangeAssignment(issueID string, userID string, sendTo string) (issueMessage, oldOwner string, err error)
	// GetUserName returns the readable username from userID
	GetUserName(userID string) string
}

// Plugin implements the interface expected by the Mattermost server to communicate between the server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	BotUserID string

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration

	listManager ListManager

	telemetryClient telemetry.Client
	tracker         telemetry.Tracker
}

func (p *Plugin) OnActivate() error {
	config := p.getConfiguration()
	if err := config.IsValid(); err != nil {
		return err
	}

	botID, err := p.Helpers.EnsureBot(&model.Bot{
		Username:    "todo",
		DisplayName: "Todo Bot",
		Description: "Todo 플러그인에 의해 생성됨",
	})
	if err != nil {
		return errors.Wrap(err, "failed to ensure todo bot")
	}
	p.BotUserID = botID

	p.listManager = NewListManager(p.API)

	p.telemetryClient, err = telemetry.NewRudderClient()
	if err != nil {
		p.API.LogWarn("telemetry client not started", "error", err.Error())
	}

	return p.API.RegisterCommand(getCommand())
}

func (p *Plugin) OnDeactivate() error {
	if p.telemetryClient != nil {
		err := p.telemetryClient.Close()
		if err != nil {
			p.API.LogWarn("OnDeactivate: failed to close telemetryClient", "error", err.Error())
		}
	}

	return nil
}

// ServeHTTP demonstrates a plugin that handles HTTP requests by greeting the world.
func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/add":
		p.handleAdd(w, r)
	case "/list":
		p.handleList(w, r)
	case "/remove":
		p.handleRemove(w, r)
	case "/complete":
		p.handleComplete(w, r)
	case "/accept":
		p.handleAccept(w, r)
	case "/bump":
		p.handleBump(w, r)
	case "/telemetry":
		p.handleTelemetry(w, r)
	case "/config":
		p.handleConfig(w, r)
	case "/edit":
		p.handleEdit(w, r)
	case "/change_assignment":
		p.handleChangeAssignment(w, r)
	default:
		http.NotFound(w, r)
	}
}

type telemetryAPIRequest struct {
	Event      string
	Properties map[string]interface{}
}

func (p *Plugin) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var telemetryRequest *telemetryAPIRequest
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&telemetryRequest)
	if err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	if telemetryRequest.Event != "" {
		p.trackFrontend(userID, telemetryRequest.Event, telemetryRequest.Properties)
	}
}

type addAPIRequest struct {
	Message     string `json:"message"`
	Description string `json:"description"`
	SendTo      string `json:"send_to"`
	PostID      string `json:"post_id"`
}

func (p *Plugin) handleAdd(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var addRequest *addAPIRequest
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&addRequest)
	if err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	senderName := p.listManager.GetUserName(userID)

	if addRequest.SendTo == "" {
		_, err = p.listManager.AddIssue(userID, addRequest.Message, addRequest.Description, addRequest.PostID)
		if err != nil {
			p.API.LogError("Unable to add issue err=" + err.Error())
			p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to add issue", err)
			return
		}

		p.trackAddIssue(userID, sourceWebapp, addRequest.PostID != "")

		p.sendRefreshEvent(userID, []string{MyListKey})

		replyMessage := fmt.Sprintf("@%s이(가) 이 스레드에 Todo를 생성했습니다", senderName)
		p.postReplyIfNeeded(addRequest.PostID, replyMessage, addRequest.Message)

		return
	}

	receiver, appErr := p.API.GetUserByUsername(addRequest.SendTo)
	if appErr != nil {
		p.API.LogError("username not valid, err=" + appErr.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to find user", appErr)
		return
	}

	if receiver.Id == userID {
		_, err = p.listManager.AddIssue(userID, addRequest.Message, addRequest.Description, addRequest.PostID)
		if err != nil {
			p.API.LogError("Unable to add issue err=" + err.Error())
			p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to add issue", err)
			return
		}

		p.trackAddIssue(userID, sourceWebapp, addRequest.PostID != "")

		p.sendRefreshEvent(userID, []string{MyListKey})

		replyMessage := fmt.Sprintf("@%s이(가) 이 스레드에 Todo를 생성했습니다.", senderName)
		p.postReplyIfNeeded(addRequest.PostID, replyMessage, addRequest.Message)
		return
	}

	receiverAllowIncomingTaskRequestsPreference, err := p.getAllowIncomingTaskRequestsPreference(receiver.Id)
	if err != nil {
		p.API.LogError("Error when getting allow incoming task request preference, err=", err)
		receiverAllowIncomingTaskRequestsPreference = true
	}
	if !receiverAllowIncomingTaskRequestsPreference {
		replyMessage := fmt.Sprintf("@%s은(는) Todo 요청을 차단했습니다.", receiver.Username)
		p.PostBotDM(userID, replyMessage)
		return
	}

	issueID, err := p.listManager.SendIssue(userID, receiver.Id, addRequest.Message, addRequest.Description, addRequest.PostID)
	if err != nil {
		p.API.LogError("Unable to send issue err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to send issue", err)
		return
	}

	p.trackSendIssue(userID, sourceWebapp, addRequest.PostID != "")

	p.sendRefreshEvent(userID, []string{OutListKey})
	p.sendRefreshEvent(receiver.Id, []string{InListKey})

	receiverMessage := fmt.Sprintf("@%s(으)로부터 새 Todo 항목을 받았습니다.", senderName)
	p.PostBotCustomDM(receiver.Id, receiverMessage, addRequest.Message, issueID)

	replyMessage := fmt.Sprintf("@%s이(가) @%s에게 이 스레드에서 대한 Todo를 보냈습니다", senderName, addRequest.SendTo)
	p.postReplyIfNeeded(addRequest.PostID, replyMessage, addRequest.Message)
}

func (p *Plugin) postReplyIfNeeded(postID, message, todo string) {
	if postID != "" {
		err := p.ReplyPostBot(postID, message, todo)
		if err != nil {
			p.API.LogError(err.Error())
		}
	}
}

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	listInput := r.URL.Query().Get("list")
	listID := MyListKey
	switch listInput {
	case OutFlag:
		listID = OutListKey
	case InFlag:
		listID = InListKey
	}

	issues, err := p.listManager.GetIssueList(userID, listID)
	if err != nil {
		p.API.LogError("Unable to get issues for user err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to get issues for user", err)
		return
	}

	if len(issues) > 0 && r.URL.Query().Get("reminder") == "true" && p.getReminderPreference(userID) {
		var lastReminderAt int64
		lastReminderAt, err = p.getLastReminderTimeForUser(userID)
		if err != nil {
			p.API.LogError("Unable to send reminder err=" + err.Error())
			p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to send reminder", err)
			return
		}

		var timezone *time.Location
		offset, _ := strconv.Atoi(r.Header.Get("X-Timezone-Offset"))
		timezone = time.FixedZone("local", -60*offset)

		// Post reminder message if it's the next day and been more than an hour since the last post
		now := model.GetMillis()
		nt := time.Unix(now/1000, 0).In(timezone)
		lt := time.Unix(lastReminderAt/1000, 0).In(timezone)
		if nt.Sub(lt).Hours() >= 1 && (nt.Day() != lt.Day() || nt.Month() != lt.Month() || nt.Year() != lt.Year()) {
			p.PostBotDM(userID, "일일 리마인더:\n\n"+issuesListToString(issues))
			p.trackDailySummary(userID)
			err = p.saveLastReminderTimeForUser(userID)
			if err != nil {
				p.API.LogError("Unable to save last reminder for user err=" + err.Error())
			}
		}
	}

	issuesJSON, err := json.Marshal(issues)
	if err != nil {
		p.API.LogError("Unable marhsal issues list to json err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable marhsal issues list to json", err)
		return
	}

	_, err = w.Write(issuesJSON)
	if err != nil {
		p.API.LogError("Unable to write json response err=" + err.Error())
	}
}

type editAPIRequest struct {
	ID          string `json:"id"`
	Message     string `json:"message"`
	Description string `json:"description"`
}

func (p *Plugin) handleEdit(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var editRequest *editAPIRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&editRequest); err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}
	r.Body.Close()

	foreignUserID, list, oldMessage, err := p.listManager.EditIssue(userID, editRequest.ID, editRequest.Message, editRequest.Description)
	if err != nil {
		p.API.LogError("Unable to edit message: err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to edit issue", err)
		return
	}

	p.trackEditIssue(userID)
	p.sendRefreshEvent(userID, []string{list})

	if foreignUserID != "" {
		var lists []string
		if list == OutListKey {
			lists = []string{MyListKey, InListKey}
		} else {
			lists = []string{OutListKey}
		}
		p.sendRefreshEvent(foreignUserID, lists)

		userName := p.listManager.GetUserName(userID)
		message := fmt.Sprintf("@%s이(가) Todo를 수정했습니다.\n수정 전:\n%s\n수정 후:\n%s", userName, oldMessage, editRequest.Message)
		p.PostBotDM(foreignUserID, message)
	}
}

type changeAssignmentAPIRequest struct {
	ID     string `json:"id"`
	SendTo string `json:"send_to"`
}

func (p *Plugin) handleChangeAssignment(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var changeRequest *changeAssignmentAPIRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&changeRequest); err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}
	r.Body.Close()

	if changeRequest.SendTo == "" {
		http.Error(w, "No user specified", http.StatusBadRequest)
		return
	}

	receiver, appErr := p.API.GetUserByUsername(changeRequest.SendTo)
	if appErr != nil {
		p.API.LogError("username not valid, err=" + appErr.Error())
		p.handleErrorWithCode(w, http.StatusNotFound, "Unable to find user", appErr)
		return
	}

	issueMessage, oldOwner, err := p.listManager.ChangeAssignment(changeRequest.ID, userID, receiver.Id)
	if err != nil {
		p.API.LogError("Unable to change the assignment of an issue: err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to change the assignment", err)
		return
	}

	p.trackChangeAssignment(userID)

	p.sendRefreshEvent(userID, []string{MyListKey, OutListKey})

	userName := p.listManager.GetUserName(userID)
	if receiver.Id != userID {
		p.sendRefreshEvent(receiver.Id, []string{InListKey})
		receiverMessage := fmt.Sprintf("@%s(으)로부터 새 Todo를 받았습니다.", userName)
		p.PostBotCustomDM(receiver.Id, receiverMessage, issueMessage, changeRequest.ID)
	}
	if oldOwner != "" {
		p.sendRefreshEvent(oldOwner, []string{InListKey, MyListKey})
		oldOwnerMessage := fmt.Sprintf("@%s(이)가 다음 Todo 담당자에서 나를 제외시켰습니다:\n%s", userName, issueMessage)
		p.PostBotDM(oldOwner, oldOwnerMessage)
	}
}

type acceptAPIRequest struct {
	ID string `json:"id"`
}

func (p *Plugin) handleAccept(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var acceptRequest *acceptAPIRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&acceptRequest); err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	todoMessage, sender, err := p.listManager.AcceptIssue(userID, acceptRequest.ID)
	if err != nil {
		p.API.LogError("Unable to accept issue err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to accept issue", err)
		return
	}

	p.trackAcceptIssue(userID)

	p.sendRefreshEvent(userID, []string{MyListKey, InListKey})
	p.sendRefreshEvent(sender, []string{OutListKey})

	userName := p.listManager.GetUserName(userID)
	message := fmt.Sprintf("@%s이(가) 내가 보낸 Todo를 수락했습니다: %s", userName, todoMessage)
	p.PostBotDM(sender, message)
}

type completeAPIRequest struct {
	ID string `json:"id"`
}

func (p *Plugin) handleComplete(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var completeRequest *completeAPIRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&completeRequest); err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	issue, foreignID, listToUpdate, err := p.listManager.CompleteIssue(userID, completeRequest.ID)
	if err != nil {
		p.API.LogError("Unable to complete issue err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to complete issue", err)
		return
	}

	p.sendRefreshEvent(userID, []string{listToUpdate})

	p.trackCompleteIssue(userID)

	userName := p.listManager.GetUserName(userID)
	replyMessage := fmt.Sprintf("@%s이(가) 이 스레드에 연결된 Todo를 완료했습니다", userName)
	p.postReplyIfNeeded(issue.PostID, replyMessage, issue.Message)

	if foreignID == "" {
		return
	}

	p.sendRefreshEvent(foreignID, []string{OutListKey})

	message := fmt.Sprintf("@%s이(가) 내가 보낸 Todo를 완료했습니다: %s", userName, issue.Message)
	p.PostBotDM(foreignID, message)
}

type removeAPIRequest struct {
	ID string `json:"id"`
}

func (p *Plugin) handleRemove(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var removeRequest *removeAPIRequest
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&removeRequest)
	if err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	issue, foreignID, isSender, listToUpdate, err := p.listManager.RemoveIssue(userID, removeRequest.ID)
	if err != nil {
		p.API.LogError("Unable to remove issue, err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to remove issue", err)
		return
	}
	p.sendRefreshEvent(userID, []string{listToUpdate})

	p.trackRemoveIssue(userID)

	userName := p.listManager.GetUserName(userID)
	replyMessage := fmt.Sprintf("@%s이(가) 이 스레드에 첨부된 Todo를 제거했습니다", userName)
	p.postReplyIfNeeded(issue.PostID, replyMessage, issue.Message)

	if foreignID == "" {
		return
	}

	list := InListKey

	message := fmt.Sprintf("@%s이(가) 내가 받은 Todo를 제거했습니다: %s", userName, issue.Message)
	if isSender {
		message = fmt.Sprintf("@%s이(가) 내가 보낸 Todo를 거절했습니다: %s", userName, issue.Message)
		list = OutListKey
	}

	p.sendRefreshEvent(foreignID, []string{list})

	p.PostBotDM(foreignID, message)
}

type bumpAPIRequest struct {
	ID string `json:"id"`
}

func (p *Plugin) handleBump(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var bumpRequest *bumpAPIRequest
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&bumpRequest)
	if err != nil {
		p.API.LogError("Unable to decode JSON err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusBadRequest, "Unable to decode JSON", err)
		return
	}

	todoMessage, foreignUser, foreignIssueID, err := p.listManager.BumpIssue(userID, bumpRequest.ID)
	if err != nil {
		p.API.LogError("Unable to bump issue, err=" + err.Error())
		p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to bump issue", err)
		return
	}

	p.trackBumpIssue(userID)

	if foreignUser == "" {
		return
	}

	p.sendRefreshEvent(foreignUser, []string{InListKey})

	userName := p.listManager.GetUserName(userID)
	message := fmt.Sprintf("@%s이(가) 내가 받은 Todo를 강조했습니다.", userName)
	p.PostBotCustomDM(foreignUser, message, todoMessage, foreignIssueID)
}

// API endpoint to retrieve plugin configurations
func (p *Plugin) handleConfig(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	if p.configuration != nil {
		// retrieve client only configurations
		clientConfig := struct {
			HideTeamSidebar bool `json:"hide_team_sidebar"`
		}{
			HideTeamSidebar: p.configuration.HideTeamSidebar,
		}

		configJSON, err := json.Marshal(clientConfig)
		if err != nil {
			p.API.LogError("Unable to marshal plugin configuration to json err=" + err.Error())
			p.handleErrorWithCode(w, http.StatusInternalServerError, "Unable to marshal plugin configuration to json", err)
			return
		}

		_, err = w.Write(configJSON)
		if err != nil {
			p.API.LogError("Unable to write json response err=" + err.Error())
		}
	}
}

func (p *Plugin) sendRefreshEvent(userID string, lists []string) {
	p.API.PublishWebSocketEvent(
		WSEventRefresh,
		map[string]interface{}{"lists": lists},
		&model.WebsocketBroadcast{UserId: userID},
	)
}

// Publish a WebSocket event to update the client config of the plugin on the webapp end.
func (p *Plugin) sendConfigUpdateEvent() {
	clientConfigMap := map[string]interface{}{
		"hide_team_sidebar": p.configuration.HideTeamSidebar,
	}

	p.API.PublishWebSocketEvent(
		WSEventConfigUpdate,
		clientConfigMap,
		&model.WebsocketBroadcast{},
	)
}

func (p *Plugin) handleErrorWithCode(w http.ResponseWriter, code int, errTitle string, err error) {
	w.WriteHeader(code)
	b, _ := json.Marshal(struct {
		Error   string `json:"error"`
		Details string `json:"details"`
	}{
		Error:   errTitle,
		Details: err.Error(),
	})
	_, _ = w.Write(b)
}
