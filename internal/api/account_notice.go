package api

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"felis.lolicon.best/internal/metrics"
)

// Account change notices tell the owner of an account, at the verified address,
// that a way into it was just added, removed or moved: a passkey registered or
// removed, the email replaced (that notice goes to the OLD address, which is the
// one the owner still reads if someone else made the change). They carry the time
// and the source address and say what to do if the change was not theirs.
// Best effort, like the lock notice: the change already happened.

// notifyAccountChange mails one notice to the given address.
func (a *API) notifyAccountChange(r *http.Request, to, subject, body string) {
	if to == "" {
		return
	}
	sender, ok := a.Mailer.(noticeSender)
	if !ok {
		log.Printf("auth: no notice mailer; account change notice %q was not sent (request_id=%s)",
			subject, requestIDFromContext(r.Context()))
		return
	}
	if ok, _ := a.mailGate().take(mailGateKey); !ok {
		metrics.MailTotal.WithLabelValues("notice", "throttled").Inc()
		log.Printf("auth: mail budget spent; account change notice %q was not sent (request_id=%s)",
			subject, requestIDFromContext(r.Context()))
		return
	}
	if err := sender.SendNotice(r.Context(), to, subject, body); err != nil {
		metrics.MailTotal.WithLabelValues("notice", "failed").Inc()
		log.Printf("auth: account change notice failed (request_id=%s): %v", requestIDFromContext(r.Context()), err)
		return
	}
	metrics.MailTotal.WithLabelValues("notice", "sent").Inc()
}

// verifiedEmail is where a notice about p's account goes: the address it proved,
// or nothing.
func verifiedEmail(p *Principal) string {
	if !p.EmailVerified {
		return ""
	}
	return p.Email
}

func (a *API) notifyPasskeyAdded(r *http.Request, p *Principal) {
	subject, body := accountChangeNotice(
		"已添加 Passkey", "passkey added",
		"你的 Felis 账户刚刚添加了一个 Passkey。", "A passkey was just added to your Felis account.",
		"删除这个 Passkey", "remove that passkey",
		a.now(), a.noticeIP(r))
	a.notifyAccountChange(r, verifiedEmail(p), subject, body)
}

func (a *API) notifyPasskeyRemoved(r *http.Request, p *Principal) {
	subject, body := accountChangeNotice(
		"已删除 Passkey", "passkey removed",
		"你的 Felis 账户刚刚删除了一个 Passkey，其它设备上的登录已全部退出。",
		"A passkey was just removed from your Felis account, and every other device was signed out.",
		"检查剩下的 Passkey", "check the passkeys that remain",
		a.now(), a.noticeIP(r))
	a.notifyAccountChange(r, verifiedEmail(p), subject, body)
}

// notifyEmailChanged tells the previous verified address where the account's
// mail now goes, masked so the notice does not hand the new address to whoever
// reads the old mailbox.
func (a *API) notifyEmailChanged(r *http.Request, oldEmail, newEmail string) {
	masked := maskEmail(newEmail)
	subject, body := accountChangeNotice(
		"邮箱已更换", "email changed",
		"你的 Felis 账户的邮箱刚刚更换为 "+masked+"，这个地址以后不会再收到登录验证码。",
		"The email on your Felis account was just changed to "+masked+". This address will no longer receive sign-in codes.",
		"把邮箱改回来", "change the email back",
		a.now(), a.noticeIP(r))
	a.notifyAccountChange(r, oldEmail, subject, body)
}

func (a *API) noticeIP(r *http.Request) string {
	if ip := a.clientIP(r); ip.IsValid() {
		return ip.String()
	}
	return ""
}

// accountChangeNotice renders a bilingual notice. zhUndo/enUndo name the step
// that reverses the change, for the "if this wasn't you" line.
func accountChangeNotice(zhTitle, enTitle, zhWhat, enWhat, zhUndo, enUndo string, at time.Time, ip string) (subject, body string) {
	when := at.UTC().Format("2006-01-02 15:04 MST")
	zhIP, enIP := ip, ip
	if ip == "" {
		zhIP, enIP = "未知", "unknown"
	}
	subject = "Felis " + zhTitle + " · " + enTitle
	body = fmt.Sprintf(`%s
时间：%s
来源 IP：%s
如果不是你本人操作，请立即登录 Felis，在账户页%s并退出其它设备，然后联系服务器管理员。

%s
Time: %s
From IP: %s
If this wasn't you, sign in to Felis now, %s and sign out other devices on the Account page, then contact the server operator.
`, zhWhat, when, zhIP, zhUndo, enWhat, when, enIP, enUndo)
	return subject, body
}

// maskEmail keeps the first character of the local part and the domain:
// alice@example.com → a***@example.com.
func maskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	first := []rune(email[:at])[0]
	return string(first) + "***" + email[at:]
}
