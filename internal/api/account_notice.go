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
// that a way into it, or what it owns, was just added, removed or moved: a passkey
// registered or removed, the email replaced (that notice goes to the OLD address,
// which is the one the owner still reads if someone else made the change), a
// migration code issued against it or redeemed. They carry the time and the source
// address and say what to do if the change was not theirs.
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

// notifyMigrateCodeIssued tells the source account's owner that a code now stands to
// hand its servers to target. A code the owner did not issue still has to be redeemed,
// and running /felis migrate again voids it.
func (a *API) notifyMigrateCodeIssued(r *http.Request, to, target string, expires time.Time) {
	exp := expires.UTC().Format("2006-01-02 15:04 MST")
	subject, body := renderNotice(
		"已签发迁移码", "migration code issued",
		"你的 Felis 账户刚刚签发了迁移码。账户「"+target+"」在 "+exp+" 前兑换后，你名下的全部服务器会转给它，本账户随即停用。",
		"A migration code was just issued on your Felis account. If the account \""+target+"\" redeems it before "+exp+", every server you own moves to it and this account is retired.",
		"请立即在游戏里重新执行 /felis migrate 让这个迁移码作废，再登录 Felis 在账户页退出其它设备，然后联系服务器管理员。",
		"run /felis migrate in game right away to void this code, sign in to Felis and sign out other devices on the Account page, then contact the server operator.",
		a.now(), a.noticeIP(r))
	a.notifyAccountChange(r, to, subject, body)
}

// notifyMigrateRedeemed tells the retired source account where its servers went. The
// account can no longer sign in, so the notice goes to the address it had proved.
func (a *API) notifyMigrateRedeemed(r *http.Request, sourceUserID string, target *Principal, moved []string) {
	src, err := a.Repo.UserDetail(r.Context(), sourceUserID)
	if err != nil {
		log.Printf("auth: migration notice to the source account was not sent (request_id=%s): %v",
			requestIDFromContext(r.Context()), err)
		return
	}
	if !src.EmailVerified {
		return
	}
	zhWhat := "你的 Felis 账户刚刚迁移给了账户「" + target.Username + "」，本账户已停用，所有登录已退出。"
	enWhat := "Your Felis account was just migrated to the account \"" + target.Username + "\". This account is retired and every device was signed out."
	if len(moved) > 0 {
		list := strings.Join(moved, ", ")
		zhWhat += fmt.Sprintf("转过去的 %d 台服务器：%s。", len(moved), list)
		enWhat += fmt.Sprintf(" The %d servers that moved: %s.", len(moved), list)
	}
	subject, body := renderNotice("服务器已迁出", "servers migrated away", zhWhat, enWhat,
		"请立即联系服务器管理员。", "contact the server operator right away.",
		a.now(), a.noticeIP(r))
	a.notifyAccountChange(r, src.Email, subject, body)
}

// accountChangeNotice renders a bilingual notice. zhUndo/enUndo name the step
// that reverses the change, for the "if this wasn't you" line.
func accountChangeNotice(zhTitle, enTitle, zhWhat, enWhat, zhUndo, enUndo string, at time.Time, ip string) (subject, body string) {
	return renderNotice(zhTitle, enTitle, zhWhat, enWhat,
		"请立即登录 Felis，在账户页"+zhUndo+"并退出其它设备，然后联系服务器管理员。",
		"sign in to Felis now, "+enUndo+" and sign out other devices on the Account page, then contact the server operator.",
		at, ip)
}

// renderNotice lays out a bilingual notice; zhIfNot/enIfNot finish the "if this
// wasn't you" line.
func renderNotice(zhTitle, enTitle, zhWhat, enWhat, zhIfNot, enIfNot string, at time.Time, ip string) (subject, body string) {
	when := at.UTC().Format("2006-01-02 15:04 MST")
	zhIP, enIP := ip, ip
	if ip == "" {
		zhIP, enIP = "未知", "unknown"
	}
	subject = "Felis " + zhTitle + " · " + enTitle
	body = fmt.Sprintf(`%s
时间：%s
来源 IP：%s
如果不是你本人操作，%s

%s
Time: %s
From IP: %s
If this wasn't you, %s
`, zhWhat, when, zhIP, zhIfNot, enWhat, when, enIP, enIfNot)
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
