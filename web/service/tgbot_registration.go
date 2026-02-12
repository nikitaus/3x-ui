package service

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"

	"github.com/google/uuid"
	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// pendingRegApproval maps admin chatId to the regID being approved (for custom email input).
var pendingRegApproval = make(map[int64]string)

// getRegistration fetches a pending registration by its DB ID.
func (t *Tgbot) getRegistration(regIDStr string) (*model.Registration, error) {
	regID, err := strconv.Atoi(regIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid registration ID: %s", regIDStr)
	}
	db := database.GetDB()
	reg := &model.Registration{}
	err = db.Where("id = ? AND status = ?", regID, "pending").First(reg).Error
	if err != nil {
		return nil, err
	}
	return reg, nil
}

// markRegistration updates the status of a registration in the DB.
func (t *Tgbot) markRegistration(regID int, status string) error {
	db := database.GetDB()
	return db.Model(&model.Registration{}).Where("id = ?", regID).Update("status", status).Error
}

// buildRegButtons builds the inline keyboard for admin registration actions.
func (t *Tgbot) buildRegButtons(reqID string) *telego.InlineKeyboardMarkup {
	return tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regApprove")).WithCallbackData(t.encodeQuery("approve_reg "+reqID)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regLink")).WithCallbackData(t.encodeQuery("link_reg "+reqID)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regReject")).WithCallbackData(t.encodeQuery("reject_reg "+reqID)),
		),
	)
}

// startRegistrationFlow initiates the registration flow for a non-admin user.
func (t *Tgbot) startRegistrationFlow(chatId int64, from *telego.User) {
	userStates[chatId] = "awaiting_reg_name"
	t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.regEnterName", "Firstname=="+from.FirstName))
}

// handleRegistrationName processes the name entered by the user and notifies admins.
func (t *Tgbot) handleRegistrationName(chatId int64, from *telego.User, name string) {
	db := database.GetDB()

	reg := &model.Registration{
		TgID:      from.ID,
		ChatID:    chatId,
		Username:  from.Username,
		FirstName: from.FirstName,
		LastName:  from.LastName,
		Name:      name,
		Status:    "pending",
		CreatedAt: time.Now().Unix(),
	}

	if err := db.Create(reg).Error; err != nil {
		logger.Errorf("Failed to save registration: %v", err)
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.regSaveError"))
		return
	}

	reqID := strconv.Itoa(reg.Id)

	t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.regSubmitted"))

	adminMsg := t.I18nBot("tgbot.messages.regNewRequest",
		"Name=="+name,
		"Username=="+from.Username,
		"TgID=="+strconv.FormatInt(from.ID, 10),
	)

	inlineKeyboard := t.buildRegButtons(reqID)

	for _, adminId := range adminIds {
		t.SendMsgToTgbot(adminId, adminMsg, inlineKeyboard)
	}
}

// approveRegistration shows the email confirmation step before creating a client.
func (t *Tgbot) approveRegistration(chatId int64, reqIDStr string, callbackID string, messageID int) {
	reg, err := t.getRegistration(reqIDStr)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.messages.regNotFound"))
		return
	}

	t.sendCallbackAnswerTgBot(callbackID, "")

	email := t.generateUniqueEmail(reg.Name)

	msg := t.I18nBot("tgbot.messages.regConfirmEmail", "Email=="+email)
	inlineKeyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regConfirmEmail")).WithCallbackData(t.encodeQuery("confirm_reg "+reqIDStr+" "+email)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regChangeEmail")).WithCallbackData(t.encodeQuery("change_reg_email "+reqIDStr)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regBack")).WithCallbackData(t.encodeQuery("link_reg_cancel "+reqIDStr)),
		),
	)

	t.editMessageTgBot(chatId, messageID, msg, inlineKeyboard)
}

// confirmRegistration creates the client with the given email after admin confirmation.
func (t *Tgbot) confirmRegistration(chatId int64, reqIDStr string, email string, callbackID string, messageID int) {
	reg, err := t.getRegistration(reqIDStr)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.messages.regNotFound"))
		return
	}

	// Check if email is already taken
	_, existingClient, _ := t.inboundService.GetClientByEmail(email)
	if existingClient != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.messages.regEmailTaken", "Email=="+email))
		return
	}

	// Find a suitable inbound
	inbounds, err := t.inboundService.GetAllInbounds()
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.answers.getInboundsFailed"))
		return
	}

	var targetInbound *model.Inbound
	for _, inbound := range inbounds {
		if !inbound.Enable {
			continue
		}
		switch inbound.Protocol {
		case model.VMESS, model.VLESS, model.Trojan, model.Shadowsocks:
			targetInbound = inbound
		}
		if targetInbound != nil {
			break
		}
	}

	if targetInbound == nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.messages.regNoInbound"))
		return
	}

	clientUUID := uuid.New().String()
	subID := t.randomLowerAndNum(16)
	tgIDStr := strconv.FormatInt(reg.TgID, 10)

	clientJSON, err := t.buildRegistrationClientJSON(targetInbound.Protocol, clientUUID, email, tgIDStr, subID, reg.Name)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.answers.errorOperation"))
		return
	}

	newInbound := &model.Inbound{
		Id:       targetInbound.Id,
		Settings: clientJSON,
	}

	_, err = t.inboundService.AddInboundClient(newInbound)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.answers.errorOperation"))
		return
	}

	if err := t.markRegistration(reg.Id, "approved"); err != nil {
		logger.Errorf("Failed to mark registration %d as approved: %v", reg.Id, err)
	}

	t.editMessageTgBot(chatId, messageID,
		t.I18nBot("tgbot.messages.regClientCreated",
			"Email=="+email,
			"Username=="+reg.Username,
			"Inbound=="+targetInbound.Remark))

	t.SendMsgToTgbot(reg.ChatID, t.I18nBot("tgbot.messages.regApproved"))
	t.sendClientSubLinks(reg.ChatID, email)
	t.sendClientIndividualLinks(reg.ChatID, email)
}

// handleRegistrationEmail processes custom email input from admin.
func (t *Tgbot) handleRegistrationEmail(chatId int64, email string) {
	reqIDStr, exists := pendingRegApproval[chatId]
	if !exists {
		return
	}
	delete(pendingRegApproval, chatId)

	reg, err := t.getRegistration(reqIDStr)
	if err != nil {
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.regNotFound"))
		return
	}

	// Check if email is already taken
	_, existingClient, _ := t.inboundService.GetClientByEmail(email)
	if existingClient != nil {
		// Email taken — ask again
		pendingRegApproval[chatId] = reqIDStr
		userStates[chatId] = "awaiting_reg_email"
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.regEmailTaken", "Email=="+email))
		return
	}

	// Show confirmation with the custom email
	msg := t.I18nBot("tgbot.messages.regConfirmEmail", "Email=="+email)
	inlineKeyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regConfirmEmail")).WithCallbackData(t.encodeQuery("confirm_reg "+reqIDStr+" "+email)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regChangeEmail")).WithCallbackData(t.encodeQuery("change_reg_email "+reqIDStr)),
		),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regBack")).WithCallbackData(t.encodeQuery("link_reg_cancel "+reqIDStr)),
		),
	)

	_ = reg
	t.SendMsgToTgbot(chatId, msg, inlineKeyboard)
}

// promptRegEmail asks admin to enter a custom email for the registration.
func (t *Tgbot) promptRegEmail(chatId int64, reqIDStr string, callbackID string) {
	_, err := t.getRegistration(reqIDStr)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.messages.regNotFound"))
		return
	}

	t.sendCallbackAnswerTgBot(callbackID, "")

	pendingRegApproval[chatId] = reqIDStr
	userStates[chatId] = "awaiting_reg_email"
	t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.regEnterEmail"))
}

// showLinkOptions shows a list of existing clients to link the registration to.
func (t *Tgbot) showLinkOptions(chatId int64, reqIDStr string, callbackID string) {
	_, err := t.getRegistration(reqIDStr)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.messages.regNotFound"))
		return
	}

	t.sendCallbackAnswerTgBot(callbackID, "")

	inbounds, err := t.inboundService.GetAllInbounds()
	if err != nil {
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.getClientsFailed"))
		return
	}

	var rows [][]telego.InlineKeyboardButton
	for _, inbound := range inbounds {
		if !inbound.Enable {
			continue
		}
		clients, err := t.inboundService.GetClients(inbound)
		if err != nil {
			continue
		}
		for _, client := range clients {
			if client.TgID != 0 {
				continue // already linked
			}
			label := fmt.Sprintf("%s (%s)", client.Email, inbound.Remark)
			callbackData := t.encodeQuery(fmt.Sprintf("link_reg_to %s %s", reqIDStr, client.Email))
			rows = append(rows, tu.InlineKeyboardRow(
				tu.InlineKeyboardButton(label).WithCallbackData(callbackData),
			))
		}
	}

	if len(rows) == 0 {
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.regNoClients"))
		return
	}

	// Add cancel button
	rows = append(rows, tu.InlineKeyboardRow(
		tu.InlineKeyboardButton(t.I18nBot("tgbot.buttons.regBack")).WithCallbackData(t.encodeQuery("link_reg_cancel "+reqIDStr)),
	))

	keyboard := &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
	t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.regChooseClient"), keyboard)
}

// linkRegistration links a pending registration to an existing client by setting tgId.
func (t *Tgbot) linkRegistration(chatId int64, reqIDStr string, email string, callbackID string, messageID int) {
	reg, err := t.getRegistration(reqIDStr)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.messages.regNotFound"))
		return
	}

	// Find the client by email and set tgId
	traffic, client, err := t.inboundService.GetClientByEmail(email)
	if err != nil || client == nil || traffic == nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.answers.errorOperation"))
		return
	}

	_, err = t.inboundService.SetClientTelegramUserID(traffic.Id, reg.TgID)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.answers.errorOperation"))
		return
	}

	// Mark registration as approved in the DB
	if err := t.markRegistration(reg.Id, "approved"); err != nil {
		logger.Errorf("Failed to mark registration %d as approved: %v", reg.Id, err)
	}

	// Update admin message
	t.editMessageTgBot(chatId, messageID,
		t.I18nBot("tgbot.messages.regLinked", "Username=="+reg.Username, "Email=="+email))

	// Notify the user
	t.SendMsgToTgbot(reg.ChatID, t.I18nBot("tgbot.messages.regApproved"))
	t.sendClientSubLinks(reg.ChatID, email)
	t.sendClientIndividualLinks(reg.ChatID, email)
}

// rejectRegistration rejects a pending registration request.
func (t *Tgbot) rejectRegistration(chatId int64, reqIDStr string, callbackID string, messageID int) {
	reg, err := t.getRegistration(reqIDStr)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.messages.regNotFound"))
		return
	}

	// Mark registration as rejected in the DB
	if err := t.markRegistration(reg.Id, "rejected"); err != nil {
		logger.Errorf("Failed to mark registration %d as rejected: %v", reg.Id, err)
	}

	// Update admin message
	t.editMessageTgBot(chatId, messageID,
		t.I18nBot("tgbot.messages.regDenied", "Username=="+reg.Username, "Name=="+reg.Name))

	// Notify the user
	t.SendMsgToTgbot(reg.ChatID, t.I18nBot("tgbot.messages.regRejected"))
}

// resendRegistrationButtons re-sends the original approval buttons (used when cancelling link selection).
func (t *Tgbot) resendRegistrationButtons(chatId int64, reqIDStr string, callbackID string) {
	reg, err := t.getRegistration(reqIDStr)
	if err != nil {
		t.sendCallbackAnswerTgBot(callbackID, t.I18nBot("tgbot.messages.regNotFound"))
		return
	}

	t.sendCallbackAnswerTgBot(callbackID, "")

	adminMsg := t.I18nBot("tgbot.messages.regNewRequest",
		"Name=="+reg.Name,
		"Username=="+reg.Username,
		"TgID=="+strconv.FormatInt(reg.TgID, 10),
	)

	t.SendMsgToTgbot(chatId, adminMsg, t.buildRegButtons(reqIDStr))
}

// showPendingRegistrations displays all pending registration requests to the admin.
func (t *Tgbot) showPendingRegistrations(chatId int64, callbackID string) {
	t.sendCallbackAnswerTgBot(callbackID, "")

	db := database.GetDB()
	var regs []model.Registration
	err := db.Where("status = ?", "pending").Order("created_at desc").Find(&regs).Error
	if err != nil {
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.answers.errorOperation"))
		return
	}

	if len(regs) == 0 {
		t.SendMsgToTgbot(chatId, t.I18nBot("tgbot.messages.regNoRequests"))
		return
	}

	for _, reg := range regs {
		reqID := strconv.Itoa(reg.Id)

		adminMsg := t.I18nBot("tgbot.messages.regNewRequest",
			"Name=="+reg.Name,
			"Username=="+reg.Username,
			"TgID=="+strconv.FormatInt(reg.TgID, 10),
		)

		t.SendMsgToTgbot(chatId, adminMsg, t.buildRegButtons(reqID))
	}
}

// cleanExpiredRegistrations removes pending registration requests older than the given max age.
func cleanExpiredRegistrations(maxAge time.Duration) {
	db := database.GetDB()
	cutoff := time.Now().Add(-maxAge).Unix()
	result := db.Where("status = ? AND created_at < ?", "pending", cutoff).Delete(&model.Registration{})
	if result.Error != nil {
		logger.Errorf("Failed to clean expired registrations: %v", result.Error)
	} else if result.RowsAffected > 0 {
		logger.Infof("Cleaned %d expired registration(s)", result.RowsAffected)
	}
}

// generateUniqueEmail generates a unique email from a display name.
// If the base email already exists, a random suffix is appended.
func (t *Tgbot) generateUniqueEmail(name string) string {
	base := ""
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			base += string(r)
		}
	}
	if base == "" {
		base = t.randomLowerAndNum(8)
	}

	email := base
	for i := 0; i < 10; i++ {
		_, client, err := t.inboundService.GetClientByEmail(email)
		if err != nil || client == nil {
			return email
		}
		email = base + "_" + t.randomLowerAndNum(4)
	}
	return email
}

// buildRegistrationClientJSON builds the JSON settings for a new client in a specific inbound.
func (t *Tgbot) buildRegistrationClientJSON(protocol model.Protocol, clientID, email, tgID, subID, comment string) (string, error) {
	type clientData struct {
		ID       string `json:"id,omitempty"`
		Security string `json:"security,omitempty"`
		Flow     string `json:"flow,omitempty"`
		Password string `json:"password,omitempty"`
		Method   string `json:"method,omitempty"`
		Email    string `json:"email"`
		LimitIP  int    `json:"limitIp"`
		TotalGB  int64  `json:"totalGB"`
		Expiry   int64  `json:"expiryTime"`
		Enable   bool   `json:"enable"`
		TgID     string `json:"tgId"`
		SubID    string `json:"subId"`
		Comment  string `json:"comment"`
		Reset    int    `json:"reset"`
	}

	c := clientData{
		Email:   email,
		Enable:  true,
		TgID:    tgID,
		SubID:   subID,
		Comment: comment,
	}

	switch protocol {
	case model.VMESS:
		c.ID = clientID
		c.Security = "auto"
	case model.VLESS:
		c.ID = clientID
	case model.Trojan:
		c.Password = t.randomLowerAndNum(10)
	case model.Shadowsocks:
		c.Password = t.randomShadowSocksPassword()
	default:
		return "", fmt.Errorf("unsupported protocol: %s", protocol)
	}

	wrapper := map[string][]clientData{"clients": {c}}
	data, err := json.Marshal(wrapper)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
