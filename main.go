package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/kyokomi/go-docomo/docomo"
	"github.com/masahide/go-yammer/cometd"
	"github.com/masahide/go-yammer/schema"
	"github.com/masahide/go-yammer/yammer"
)

const (
	configFile = "cache.json"
)

var (
	conf        cache
	groupPrefix = byte('%')
	//mentionRe   = regexp.MustCompile(`\[\[user:[\d]+\]\]`)
	mentionRe   = regexp.MustCompile(`\[\[user:([\d]+)\]\]`)
	ignoreMesRe = regexp.MustCompile(`cc: .*`)

	tokenRe   = regexp.MustCompile(`\t+|\s+|"|,|\.|　+|\n+`)
	client    *yammer.Client
	dClient   *docomo.Client
	debug     bool
	contexts  = make(map[int]string)
	current   *schema.User
	place     = "東京"
	charactor = 20
)

// User is user struct
type User struct {
	Name string
	ID   int
}

// MentionList is mention group
type MentionList struct {
	Name  string
	Users []User
}

type cache struct {
	AccessToken  string
	DocomoAPIKey string
	MentionLists map[string]MentionList
}

func loadCache(file string) cache {
	var c cache
	f, err := os.Open(file)
	if err != nil {
		log.Fatal(err)
	}
	dec := json.NewDecoder(f)
	if err := dec.Decode(&c); err != nil {
		log.Fatal(err)
	}
	if c.MentionLists == nil {
		c.MentionLists = make(map[string]MentionList)
	}
	return c
}

func saveCache(file string, config cache) error {
	b, err := json.Marshal(config)
	if err != nil {
		log.Fatal(err)
	}
	return ioutil.WriteFile(file, b, 0600)
}

func init() {
	flag.BoolVar(&debug, "debug", debug, "debug mode")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	conf = loadCache(configFile)
	client = yammer.New(conf.AccessToken)
	client.DebugMode = debug
	if conf.DocomoAPIKey != "" {
		dClient = docomo.NewClient(conf.DocomoAPIKey)
	}
}

func main() {
	for {
		mainLoop()
	}
}

func mainLoop() {
	realtime, err := client.Realtime()
	if err != nil {
		log.Println(err)
		return
	}
	current, err = client.Current()
	if err != nil {
		log.Println(err)
		return
	}
	inbox, err := client.InboxFeedV2()
	if err != nil {
		log.Println(err)
		return
	}

	rt := cometd.New(realtime.RealtimeURI, realtime.AuthenticationToken)
	err = rt.Handshake()
	if err != nil {
		log.Println(err)
		return
	}

	rt.SubscribeToFeed(inbox.ChannelID)
	messageChan := make(chan *cometd.ConnectionResponse, 10)
	stopChan := make(chan bool)

	log.Printf("Polling Realtime channelID: %v\n", inbox.ChannelID)
	go rt.Poll(messageChan, stopChan)
	for {
		select {
		case m, ok := <-messageChan:
			if !ok {
				return
			}
			if m.Channel == "/meta/connect" {
				continue
			}
			if m.Data.Type != "message" {
				log.Printf("Data.Type is not message. channel:%#v", m)
				continue
			}
			if m.Data.Feed == nil {
				log.Printf("Data.Feed is nil. channel:%#v", m)
				continue
			}
			receiveMessage(m.Data.Feed)
		}
	}
}

func receiveMessage(feed *schema.MessageFeed) {
	for _, mes := range feed.Messages {
		analysis(*mes, feed.References)
	}
}

func analysis(mes schema.Message, refs []*schema.Reference) {
	log.Printf("ThreadId:%d -> receiveMessage: \"%s\"", mes.ThreadId, mes.Body.Parsed)
	mes.Body.Parsed = strings.Replace(mes.Body.Parsed, fmt.Sprintf("added [[user:%d]] to the conversation.", current.ID), "", -1)
	m := getMentions(mes, refs)
	if m.ToMe {
		dispatcher(mes, m)
	}
	//re.FindStringSubmatch("mes.Body.Parsed")
}

type mentions struct {
	Users []User
	ToMe  bool
	From  schema.Reference
}

func getRef(id int, refs []*schema.Reference) schema.Reference {
	for _, r := range refs {
		if r.ID == id {
			return *r
		}
	}
	return schema.Reference{}
}
func getUser(ref schema.Reference) User {
	return User{
		ID:   ref.ID,
		Name: ref.FullName,
	}
}

func getMentions(mes schema.Message, refs []*schema.Reference) mentions {
	//m := mentionRe.FindAllString(mes, -1)
	match := mentionRe.FindAllStringSubmatch(mes.Body.Parsed, -1)
	m := make([]int, 0, len(match))
	for _, n := range match {
		if len(n) >= 2 {
			if i, err := strconv.Atoi(n[1]); err == nil {
				m = append(m, i)
			}
		}
	}
	res := mentions{Users: make([]User, 0, len(m)), ToMe: false}
	for _, u := range m {
		if u == current.ID {
			res.ToMe = true
		} else {
			res.Users = append(res.Users, getUser(getRef(u, refs)))
		}
	}
	res.From = getRef(mes.SenderId, refs)
	return res
}

type botFunc struct {
	groups []string
	mes    schema.Message
	m      mentions
}

func dispatcher(mes schema.Message, m mentions) {
	tokens := scan(mes.Body.Parsed)
	b := &botFunc{
		groups: getGroups(tokens),
		mes:    mes,
		m:      m,
	}
	f := b.getAcction(mes.Body.Parsed)
	if f == nil {
		// log.Printf("ThreadId:%d -> unknown: %s", mes.ThreadId, mes.Body.Parsed)
		return
	}
	err := f()
	if err != nil {
		log.Printf("%v, err:%s", f, err)
	}

}

func scan(mes string) []string {
	return tokenRe.Split(mes, -1)
}

func getGroups(tokens []string) []string {
	groups := []string{}
	for _, t := range tokens {
		if len(t) > 1 {
			if byte(t[0]) == groupPrefix {
				groups = append(groups, t)
			}
		}
	}
	return groups
}
func (b *botFunc) getAcction(mes string) func() error {
	switch {
	case strings.Contains(mes, "追加"):
		return b.add
	case strings.Contains(mes, "グループ削除"):
		return b.delGroup
	case strings.Contains(mes, "削除"):
		return b.del
	case strings.Contains(mes, "全部表示"),
		strings.Contains(mes, "メンションリスト"):
		return b.showAll
	case strings.Contains(mes, "表示"),
		strings.Contains(mes, "メンバーは"):
		return b.show
	case strings.Contains(mes, "変更"),
		strings.Contains(mes, "リネーム"):
		return b.rename
	case strings.Contains(mes, "メンション"),
		strings.Contains(mes, "めんしょん"),
		strings.Contains(mes, "ccして"),
		strings.Contains(mes, "CCして"):
		return b.cc
	case dClient != nil:
		return b.zatu
	}
	return nil
}
func (b *botFunc) zatu() error {
	message := strings.Replace(b.mes.Body.Plain, "\n", " ", -1)
	message = strings.Replace(message, current.FullName, "", -1)
	message = ignoreMesRe.ReplaceAllString(message, "")
	message = strings.TrimSpace(message)
	if message == "" {
		return nil
	}
	zatsu := docomo.DialogueRequest{
		Utt:         &message,
		Place:       &place,
		Nickname:    &b.m.From.FirstName,
		CharactorID: &charactor,
	}
	context, ok := contexts[b.mes.ThreadId]
	if ok {
		zatsu.Context = &context
	}
	res, err := dClient.Dialogue.Get(zatsu, true)
	if err != nil {
		log.Printf("ThreadId:%d -> docomo.Dialogue err:%s, ThreadId:%d, message:'%s'", b.mes.ThreadId, err, b.mes.ThreadId, message)
		return err
	}
	log.Printf("ThreadId:%d -> dococomo.Dialogue zatu: '%s'", b.mes.ThreadId, res.Utt)
	postMes := &yammer.CreateMessageParams{Body: res.Utt, RepliedToId: b.mes.ThreadId}
	_, err = client.PostMessage(postMes)
	contexts[b.mes.ThreadId] = res.Context
	return err
}

func (b *botFunc) add() error {
	if len(b.groups) == 0 {
		return nil
	}
	group := b.groups[0]
	log.Printf("ThreadId:%d -> add mention group:%v, %v", b.mes.ThreadId, group, b.m)

	var body string
	list, ok := conf.MentionLists[group]
	if !ok {
		conf.MentionLists[group] = MentionList{
			Name:  group,
			Users: b.m.Users,
		}
		body = fmt.Sprintf("%s は存在しないので作成しました\n", group)
	} else {
		for _, u := range b.m.Users {
			list.Users = appendIfMissing(list.Users, u)
		}
		conf.MentionLists[group] = list
	}
	err := saveCache(configFile, conf)
	if err != nil {
		body += fmt.Sprintf("メンバー更新に失敗しました.\nerr:%s", err)
	} else {
		body += fmt.Sprintf("%s のメンバーは\n %v に更新されました", group, userNameJoin(conf.MentionLists[group].Users))
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: b.mes.ThreadId}
	_, err = client.PostMessage(postMes)
	return err
}
func (b *botFunc) delGroup() error {
	if len(b.groups) == 0 {
		return nil
	}
	group := b.groups[0]
	log.Printf("ThreadId:%d -> delGroup :%v, %v", b.mes.ThreadId, group, b.m)

	var body string
	_, ok := conf.MentionLists[group]
	if !ok {
		body = fmt.Sprintf("%s は存在しません\n", group)
	} else {
		delete(conf.MentionLists, group)
		body = fmt.Sprintf("%s を削除しました\n", group)
	}
	err := saveCache(configFile, conf)
	if err != nil {
		body += fmt.Sprintf("メンバー更新に失敗しました.\nerr:%s", err)
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: b.mes.ThreadId}
	_, err = client.PostMessage(postMes)
	return err
}

func (b *botFunc) del() error {
	if len(b.groups) == 0 {
		return nil
	}
	group := b.groups[0]
	log.Printf("ThreadId:%d -> del mention group:%v, %v", b.mes.ThreadId, group, b.m)
	var body string
	list, ok := conf.MentionLists[group]
	if !ok {
		log.Printf("ThreadId:%d ->not exist group:%v, %v", b.mes.ThreadId, group, b.m)
		body = fmt.Sprintf("%s は存在しません\n", group)
	} else {
		for _, u := range b.m.Users {
			list.Users = deleteIfExists(list.Users, u)
		}
		conf.MentionLists[group] = list
		err := saveCache(configFile, conf)
		if err != nil {
			body += fmt.Sprintf("メンバー更新に失敗しました.\nerr:%s", err)
		} else {
			body += fmt.Sprintf("%s のメンバーは\n %v に更新されました", group, userNameJoin(list.Users))
		}
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: b.mes.ThreadId}
	_, err := client.PostMessage(postMes)
	return err
}

func (b *botFunc) cc() error {
	if len(b.groups) == 0 {
		return nil
	}
	group := b.groups[0]
	log.Printf("ThreadId:%d -> cc mention group:%v, %v", b.mes.ThreadId, group, b.m)
	var ccIDs, directToUserIDs string
	body := fmt.Sprintf("%s は存在しません", group)
	list, ok := conf.MentionLists[group]
	if ok {
		body = fmt.Sprintf("%s にccします", group)
		ccIDs = userJoin(list.Users)
		if b.mes.DirectMessage {
			directToUserIDs = idJoin(list.Users)
		}
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: b.mes.ThreadId, CC: ccIDs, DirectToUserIDs: directToUserIDs}
	_, err := client.PostMessage(postMes)
	return err
}

func (b *botFunc) show() error {
	if len(b.groups) == 0 {
		return nil
	}
	group := b.groups[0]
	log.Printf("ThreadId:%d -> show mention group:%v, %v", b.mes.ThreadId, group, b.m)
	body := fmt.Sprintf("%s は存在しません", group)
	list, ok := conf.MentionLists[group]
	if ok {
		body = fmt.Sprintf("%s のメンバーは\n %v です", group, userNameJoin(list.Users))
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: b.mes.ThreadId}
	_, err := client.PostMessage(postMes)
	return err
}
func (b *botFunc) showAll() error {
	log.Printf("ThreadId:%d -> showAll mention. %v", b.mes.ThreadId, b.m)
	body := ""
	for group, list := range conf.MentionLists {
		body += fmt.Sprintf("%s のメンバーは\n %v です\n\n", group, userNameJoin(list.Users))
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: b.mes.ThreadId}
	_, err := client.PostMessage(postMes)
	return err
}
func (b *botFunc) rename() error {
	if len(b.groups) < 2 {
		return nil
	}
	from := b.groups[0]
	to := b.groups[1]
	log.Printf("ThreadId:%d -> show mention group:%v, %v", b.mes.ThreadId, from, b.m)
	body := ""
	log.Printf("ThreadId:%d -> rename mention group:%v, %v", b.mes.ThreadId, from, b.m)
	list, ok := conf.MentionLists[from]
	if !ok {
		body = fmt.Sprintf("%s は存在しません", from)
	} else {
		conf.MentionLists[to] = list
		delete(conf.MentionLists, from)
		body = fmt.Sprintf("%s を %s へリネームしました", from, to)
		err := saveCache(configFile, conf)
		if err != nil {
			body += fmt.Sprintf("メンバー更新に失敗しました.\nerr:%s", err)
		}
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: b.mes.ThreadId}
	_, err := client.PostMessage(postMes)
	return err
}

func idJoin(users []User) string {
	s := make([]string, len(users))
	for i := range users {
		s[i] = fmt.Sprintf("%d", users[i].ID)
	}
	return strings.Join(s, ",")
}
func userJoin(users []User) string {
	s := make([]string, len(users))
	for i := range users {
		s[i] = fmt.Sprintf("[[user:%d]]", users[i].ID)
	}
	return strings.Join(s, ",")
}

func userNameJoin(users []User) string {
	s := make([]string, len(users))
	for i := range users {
		s[i] = users[i].Name
	}
	return strings.Join(s, ",")
}

func deleteIfExists(slice []User, s User) []User {
	res := make([]User, 0, len(slice))
	for _, ele := range slice {
		if ele.ID == s.ID {
			continue
		}
		res = append(res, ele)
	}
	return res
}
func appendIfMissing(slice []User, s User) []User {
	for _, ele := range slice {
		if ele.ID == s.ID {
			return slice
		}
	}
	return append(slice, s)
}
