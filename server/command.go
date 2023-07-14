package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
)

const (
	listHeaderMessage = " Todo List:\n\n"
	MyFlag            = "my"
	InFlag            = "in"
	OutFlag           = "out"
)

func getHelp() string {
	return `지원되는 명령어:

add [메시지]
	Todo를 추가합니다.

	예: /todo add 회의록 작성 및 배포

list
	내 Todo 목록을 봅니다.

list [listName]
	특정 목록을 조회합니다

	예: /todo list in
	예: /todo list out
	예: /todo list my (/todo list와 동일)

pop
	Todo 목록의 맨 상단 항목을 제거합니다.

send [user] [message]
	[user]에게 Todo를 보냅니다.

	예: /todo send @someone 캘린더에 회의 공지해주세요 

settings summary [on, off]
	일일 리마인더에 대한 사용자 설정을 지정합니다

	예: /todo settings summary on

settings allow_incoming_task_requests [on, off]
	다른 사용자의 Todo Task 보내기 수락 여부를 설정합니다

	예: /todo settings allow_incoming_task_requests on


help
	사용법을 보여줍니다.
`
}

func getSummarySetting(flag bool) string {
	if flag {
		return "리마인더를 `on`으로 설정했습니다. **일일 리마인더를 수신하게 됩니다.**"
	}
	return "리마인더를 `off`로 설정했습니다. **일일 리마인더를 수신하지 않습니다.**"
}
func getAllowIncomingTaskRequestsSetting(flag bool) string {
	if flag {
		return "들어오는 Task 요청 허용을 `on`으로 설정했습니다. **다른 사용자가 Task 요청을 보낼 수 있습니다. 받은 요청을 수락/거부 할 수 있습니다.**"
	}
	return "들어오는 Task 요청 허용을 `off`로 설정했습니다. **다른 사용자가 Task 요청을 보낼 수 없습니다. 다른 사용자에게는 내가 Task 요청을 접수 거부했다는 메시지를 받게됩니다**"
}

func getAllSettings(summaryFlag, blockIncomingFlag bool) string {
	return fmt.Sprintf(`현재 설정:

%s
%s
	`, getSummarySetting(summaryFlag), getAllowIncomingTaskRequestsSetting(blockIncomingFlag))
}

func getCommand() *model.Command {
	return &model.Command{
		Trigger:          "todo",
		DisplayName:      "Todo Bot",
		Description:      "Todo list와 상호작용 기능을 제공합니다.",
		AutoComplete:     true,
		AutoCompleteDesc: "지원되는 명령어: add, list, pop, send, help",
		AutoCompleteHint: "[command]",
		AutocompleteData: getAutocompleteData(),
	}
}

func (p *Plugin) postCommandResponse(args *model.CommandArgs, text string) {
	post := &model.Post{
		UserId:    p.BotUserID,
		ChannelId: args.ChannelId,
		Message:   text,
	}
	_ = p.API.SendEphemeralPost(args.UserId, post)
}

// ExecuteCommand executes a given command and returns a command response.
func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	spaceRegExp := regexp.MustCompile(`\s+`)
	trimmedArgs := spaceRegExp.ReplaceAllString(strings.TrimSpace(args.Command), " ")
	stringArgs := strings.Split(trimmedArgs, " ")
	lengthOfArgs := len(stringArgs)
	restOfArgs := []string{}

	var handler func([]string, *model.CommandArgs) (bool, error)
	if lengthOfArgs == 1 {
		handler = p.runListCommand
		p.trackCommand(args.UserId, "")
	} else {
		command := stringArgs[1]
		if lengthOfArgs > 2 {
			restOfArgs = stringArgs[2:]
		}
		switch command {
		case "add":
			handler = p.runAddCommand
		case "list":
			handler = p.runListCommand
		case "pop":
			handler = p.runPopCommand
		case "send":
			handler = p.runSendCommand
		case "settings":
			handler = p.runSettingsCommand
		default:
			if command == "help" {
				p.trackCommand(args.UserId, command)
			} else {
				p.trackCommand(args.UserId, "not found")
			}
			p.postCommandResponse(args, getHelp())
			return &model.CommandResponse{}, nil
		}
		p.trackCommand(args.UserId, command)
	}
	isUserError, err := handler(restOfArgs, args)
	if err != nil {
		if isUserError {
			p.postCommandResponse(args, fmt.Sprintf("__Error: %s.__\n\n사용법을 보려면 `/todo help`를 실행하세요.", err.Error()))
		} else {
			p.API.LogError(err.Error())
			p.postCommandResponse(args, "알 수 없는 오류가 발생했습니다. 시스템 관리자에게 도움을 요청하세요.")
		}
	}

	return &model.CommandResponse{}, nil
}

func (p *Plugin) runSendCommand(args []string, extra *model.CommandArgs) (bool, error) {
	if len(args) < 2 {
		p.postCommandResponse(extra, "반드시 유효한 사용자와 메시지를 지정해야합니다.\n"+getHelp())
		return false, nil
	}

	userName := args[0]
	if args[0][0] == '@' {
		userName = args[0][1:]
	}
	receiver, appErr := p.API.GetUserByUsername(userName)
	if appErr != nil {
		p.postCommandResponse(extra, "유효한 사용자를 지정하세요.\n"+getHelp())
		return false, nil
	}

	if receiver.Id == extra.UserId {
		return p.runAddCommand(args[1:], extra)
	}

	receiverAllowIncomingTaskRequestsPreference, err := p.getAllowIncomingTaskRequestsPreference(receiver.Id)
	if err != nil {
		p.API.LogError("들어오는 Task 요청 허용에 대한 설정 확인 시 오류가 발생했습니다, err=", err)
		receiverAllowIncomingTaskRequestsPreference = true
	}
	if !receiverAllowIncomingTaskRequestsPreference {
		p.postCommandResponse(extra, fmt.Sprintf("@%s은(는) Todo 요청을 차단 중입니다", userName))
		return false, nil
	}

	message := strings.Join(args[1:], " ")

	receiverIssueID, err := p.listManager.SendIssue(extra.UserId, receiver.Id, message, "", "")
	if err != nil {
		return false, err
	}

	p.trackSendIssue(extra.UserId, sourceCommand, false)

	p.sendRefreshEvent(extra.UserId, []string{OutListKey})
	p.sendRefreshEvent(receiver.Id, []string{InListKey})

	responseMessage := fmt.Sprintf("@%s에게 Todo를 보냈습니다.", userName)

	senderName := p.listManager.GetUserName(extra.UserId)

	receiverMessage := fmt.Sprintf("@%s(으)로부터 새 Todo 요청을 받았습니다", senderName)

	p.PostBotCustomDM(receiver.Id, receiverMessage, message, receiverIssueID)
	p.postCommandResponse(extra, responseMessage)
	return false, nil
}

func (p *Plugin) runAddCommand(args []string, extra *model.CommandArgs) (bool, error) {
	message := strings.Join(args, " ")

	if message == "" {
		p.postCommandResponse(extra, "Task를 추가하세요.")
		return false, nil
	}

	newIssue, err := p.listManager.AddIssue(extra.UserId, message, "", "")
	if err != nil {
		return false, err
	}

	p.trackAddIssue(extra.UserId, sourceCommand, false)

	p.sendRefreshEvent(extra.UserId, []string{MyListKey})

	responseMessage := "Todo가 추가되었습니다."

	issues, err := p.listManager.GetIssueList(extra.UserId, MyListKey)
	if err != nil {
		p.API.LogError(err.Error())
		p.postCommandResponse(extra, responseMessage)
		return false, nil
	}

	// It's possible that database replication delay has resulted in the issue
	// list not containing the newly-added issue, so we check for that and
	// append the issue manually if necessary.
	var issueIncluded bool
	for _, issue := range issues {
		if newIssue.ID == issue.ID {
			issueIncluded = true
			break
		}
	}
	if !issueIncluded {
		issues = append(issues, &ExtendedIssue{
			Issue: *newIssue,
		})
	}

	responseMessage += listHeaderMessage
	responseMessage += issuesListToString(issues)
	p.postCommandResponse(extra, responseMessage)

	return false, nil
}

func (p *Plugin) runListCommand(args []string, extra *model.CommandArgs) (bool, error) {
	listID := MyListKey
	responseMessage := "Todo 목록:\n\n"

	if len(args) > 0 {
		switch args[0] {
		case MyFlag:
		case InFlag:
			listID = InListKey
			responseMessage = "받은 Todo 목록:\n\n"
		case OutFlag:
			listID = OutListKey
			responseMessage = "보낸 Todo 목록:\n\n"
		default:
			p.postCommandResponse(extra, getHelp())
			return true, nil
		}
	}

	issues, err := p.listManager.GetIssueList(extra.UserId, listID)
	if err != nil {
		return false, err
	}

	p.sendRefreshEvent(extra.UserId, []string{MyListKey, OutListKey, InListKey})

	responseMessage += issuesListToString(issues)
	p.postCommandResponse(extra, responseMessage)

	return false, nil
}

func (p *Plugin) runPopCommand(args []string, extra *model.CommandArgs) (bool, error) {
	issue, foreignID, err := p.listManager.PopIssue(extra.UserId)
	if err != nil {
		if err.Error() == "cannot find issue" {
			p.postCommandResponse(extra, "제거할 Todo가 없습니다.")
			return false, nil
		}
		return false, err
	}

	userName := p.listManager.GetUserName(extra.UserId)

	if foreignID != "" {
		p.sendRefreshEvent(foreignID, []string{OutListKey})

		message := fmt.Sprintf("@%s이(가) 내가 보낸 Todo를 삭제했습니다: %s", userName, issue.Message)
		p.PostBotDM(foreignID, message)
	}

	p.sendRefreshEvent(extra.UserId, []string{MyListKey})

	responseMessage := "맨 위에 있는 Todo 항목을 제거했습니다."

	replyMessage := fmt.Sprintf("@%s이(가) 이 스레드에 첨부된 Todo 맨 위 항목을 제거했습니다", userName)
	p.postReplyIfNeeded(issue.PostID, replyMessage, issue.Message)

	issues, err := p.listManager.GetIssueList(extra.UserId, MyListKey)
	if err != nil {
		p.API.LogError(err.Error())
		p.postCommandResponse(extra, responseMessage)
		return false, nil
	}

	responseMessage += listHeaderMessage
	responseMessage += issuesListToString(issues)
	p.postCommandResponse(extra, responseMessage)

	return false, nil
}

func (p *Plugin) runSettingsCommand(args []string, extra *model.CommandArgs) (bool, error) {
	const (
		on  = "on"
		off = "off"
	)
	if len(args) < 1 {
		currentSummarySetting := p.getReminderPreference(extra.UserId)
		currentAllowIncomingTaskRequestsSetting, err := p.getAllowIncomingTaskRequestsPreference(extra.UserId)
		if err != nil {
			p.API.LogError("들어오는 Task 요청 허용에 대한 설정 확인 시 오류가 발생했습니다, err=", err)
			currentAllowIncomingTaskRequestsSetting = true
		}
		p.postCommandResponse(extra, getAllSettings(currentSummarySetting, currentAllowIncomingTaskRequestsSetting))
		return false, nil
	}

	switch args[0] {
	case "summary":
		if len(args) < 2 {
			currentSummarySetting := p.getReminderPreference(extra.UserId)
			p.postCommandResponse(extra, getSummarySetting(currentSummarySetting))
			return false, nil
		}
		if len(args) > 2 {
			return true, errors.New("너무 많은 인수들입니다")
			// return true, errors.New("too many arguments")
		}
		var responseMessage string
		var err error

		switch args[1] {
		case on:
			err = p.saveReminderPreference(extra.UserId, true)
			responseMessage = "이제부터 일일 요약을 수신합니다."
		case off:
			err = p.saveReminderPreference(extra.UserId, false)
			responseMessage = "이제부터 일일 요약을 수신하지 않습니다."
		default:
			responseMessage = "유효하지 않은 입력 값입니다. \"settings summary\"에 허용되는 값은 `on` 또는 `off`입니다"
			return true, errors.New(responseMessage)
		}

		if err != nil {
			responseMessage = "리마인더 설정 저장 오류입니다"
			p.API.LogDebug("runSettingsCommand: 리마인더 설정 저장 오류입니다", "error", err.Error())
			return false, errors.New(responseMessage)
		}

		p.postCommandResponse(extra, responseMessage)

	case "allow_incoming_task_requests":
		if len(args) < 2 {
			currentAllowIncomingTaskRequestsSetting, err := p.getAllowIncomingTaskRequestsPreference(extra.UserId)
			if err != nil {
				p.API.LogError("들어오는 Task 요청 승인에 대한 설정을 읽을 수 없습니다, err=", err.Error())
				currentAllowIncomingTaskRequestsSetting = true
			}
			p.postCommandResponse(extra, getAllowIncomingTaskRequestsSetting(currentAllowIncomingTaskRequestsSetting))
			return false, nil
		}
		if len(args) > 2 {
			return true, errors.New("너무 많은 인수들입니다")
		}
		var responseMessage string
		var err error

		switch args[1] {
		case on:
			err = p.saveAllowIncomingTaskRequestsPreference(extra.UserId, true)
			responseMessage = "다른 사용자들이 Task 요청을 보낼 수 있습니다. 받은 요청은 수락/거부할 수 있습니다"
		case off:
			err = p.saveAllowIncomingTaskRequestsPreference(extra.UserId, false)
			responseMessage = "다른 사용자들이 Task 요청을 보낼 수 없습니다. 요청을 보낸 사용자는 내가 Task 요청을 접수 거부했다는 메시지를 받게됩니다."
		default:
			responseMessage = "유효하지 않은 입력, \"settings allow_incoming_task_requests\"에 허용되는 값은 `on` 또는 `off`입니다."
			return true, errors.New(responseMessage)
		}

		if err != nil {
			responseMessage = "block_incoming 설정 저장 오류"
			p.API.LogDebug("runSettingsCommand: block_incoming 설정 저장 오류", "error", err.Error())
			return false, errors.New(responseMessage)
		}

		p.postCommandResponse(extra, responseMessage)
	default:
		return true, fmt.Errorf("setting `%s` 식별되지 않습니다", args[0])
	}
	return false, nil
}

func getAutocompleteData() *model.AutocompleteData {
	todo := model.NewAutocompleteData("todo", "[command]", "지원되는 명령어: list, add, pop, send, settings, help")

	add := model.NewAutocompleteData("add", "[message]", "추가할 Todo 내용")
	add.AddTextArgument("예: 회의 준비", "[message]", "")
	todo.AddCommand(add)

	list := model.NewAutocompleteData("list", "[name]", "내 Todo 목록 보기")
	items := []model.AutocompleteListItem{{
		HelpText: "받은 Todo",
		Hint:     "(옵션)",
		Item:     "in",
	}, {
		HelpText: "보낸 Todo",
		Hint:     "(옵션)",
		Item:     "out",
	}}
	list.AddStaticListArgument("내 Todo 목록 보기", false, items)
	todo.AddCommand(list)

	pop := model.NewAutocompleteData("pop", "", "목록의 맨 위 Todo 항목을 제거")
	todo.AddCommand(pop)

	send := model.NewAutocompleteData("send", "[user] [todo]", "특정 사용자에게 Todo 보내기")
	send.AddTextArgument("받을 사용자", "[@awesomePerson]", "")
	send.AddTextArgument("Todo 메시지", "[message]", "")
	todo.AddCommand(send)

	settings := model.NewAutocompleteData("settings", "[setting] [on] [off]", "사용자 설정")
	summary := model.NewAutocompleteData("summary", "[on] [off]", "summary 설정")
	summaryOn := model.NewAutocompleteData("on", "", "일일 리마인더를 사용으로 설정")
	summaryOff := model.NewAutocompleteData("off", "", "일일 리마인더를 사용안함으로 설정")
	summary.AddCommand(summaryOn)
	summary.AddCommand(summaryOff)

	allowIncomingTask := model.NewAutocompleteData("allow_incoming_task_requests", "[on] [off]", "다른 사용자가 나에게 Task 보내기를 허용여부 설정")
	allowIncomingTaskOn := model.NewAutocompleteData("on", "", "다른 사용자는 Task 요청을 보낼 수 있고 내가 수락/거부를 결정")
	allowIncomingTaskOff := model.NewAutocompleteData("off", "", "다른 사용자의 Task 요청을 차단하고, 보낸 사용자에게는 내가 Todo Task 요청을 차단 중임을 알림")
	allowIncomingTask.AddCommand(allowIncomingTaskOn)
	allowIncomingTask.AddCommand(allowIncomingTaskOff)

	settings.AddCommand(summary)
	settings.AddCommand(allowIncomingTask)
	todo.AddCommand(settings)

	help := model.NewAutocompleteData("help", "", "사용안내")
	todo.AddCommand(help)
	return todo
}
