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

	"github.com/k0kubun/pp"
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
	mentionRe = regexp.MustCompile(`\[\[user:([\d]+)\]\]`)

	tokenRe  = regexp.MustCompile(`\t+|\s+|"|,|\.|　+|\n+`)
	client   *yammer.Client
	dClient  *docomo.Client
	debug    bool
	contexts = make(map[int]string)
	current  *schema.User
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
	dClient = docomo.NewClient(conf.DocomoAPIKey)
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
	pp.Print(inbox)
	os.Exit(0)

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
				close(stopChan)
				break
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
func dispatcher(mes schema.Message, m mentions) {
	tokens := scan(mes.Body.Parsed)
	group := getGroup(tokens)
	f := getAcction(mes.Body.Parsed)
	if f == nil {
		log.Printf("ThreadId:%d -> unknown: %s", mes.ThreadId, mes.Body.Parsed)
		return
	}
	err := f(group, mes, m)
	if err != nil {
		log.Printf("%v, err:%s", f, err)
	}

}

func scan(mes string) []string {
	return tokenRe.Split(mes, -1)
}

func getGroup(tokens []string) string {
	for _, t := range tokens {
		if len(t) > 1 {
			if byte(t[0]) == groupPrefix {
				return t
			}
		}
	}
	return ""
}
func getAcction(mes string) func(string, schema.Message, mentions) error {
	switch {
	case strings.Contains(mes, "追加"):
		return add
	case strings.Contains(mes, "削除"):
		return del
	case strings.Contains(mes, "表示"),
		strings.Contains(mes, "メンバーは"):
		return show
	case strings.Contains(mes, "メンション"),
		strings.Contains(mes, "めんしょん"),
		strings.Contains(mes, "ccして"),
		strings.Contains(mes, "CCして"):
		return cc
	}
	return zatu
}
func zatu(group string, mes schema.Message, m mentions) error {
	message := strings.Replace(mes.Body.Plain, "\n", " ", -1)
	message = strings.Replace(message, current.FullName, "", -1)
	place := "東京"
	charactor := 20
	zatsu := docomo.DialogueRequest{
		Utt:         &message,
		Place:       &place,
		Nickname:    &m.From.FirstName,
		CharactorID: &charactor,
	}
	context, ok := contexts[mes.ThreadId]
	if ok {
		zatsu.Context = &context
	}
	res, err := dClient.Dialogue.Get(zatsu, true)
	if err != nil {
		log.Printf("ThreadId:%d -> docomo.Dialogue err:%s, ThreadId:%d, message:'%s'", mes.ThreadId, err, mes.ThreadId, message)
		return err
	}
	log.Printf("ThreadId:%d -> dococomo.Dialogue zatu: '%s'", mes.ThreadId, res.Utt)
	postMes := &yammer.CreateMessageParams{Body: res.Utt, RepliedToId: mes.ThreadId}
	_, err = client.PostMessage(postMes)
	contexts[mes.ThreadId] = res.Context
	return err
}

func add(group string, mes schema.Message, m mentions) error {
	log.Printf("ThreadId:%d -> add mention group:%v, %v", mes.ThreadId, group, m)

	var body string
	list, ok := conf.MentionLists[group]
	if !ok {
		conf.MentionLists[group] = MentionList{
			Name:  group,
			Users: m.Users,
		}
		body = fmt.Sprintf("%s は存在しないので作成しました\n", group)
	} else {
		for _, u := range m.Users {
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
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: mes.ThreadId}
	_, err = client.PostMessage(postMes)
	return err
}

func del(group string, mes schema.Message, m mentions) error {
	log.Printf("ThreadId:%d -> del mention group:%v, %v", mes.ThreadId, group, m)
	var body string
	list, ok := conf.MentionLists[group]
	if !ok {
		log.Printf("ThreadId:%d ->not exist group:%v, %v", mes.ThreadId, group, m)
		body = fmt.Sprintf("%s は存在しません\n", group)
	} else {
		for _, u := range m.Users {
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
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: mes.ThreadId}
	_, err := client.PostMessage(postMes)
	return err
}
func show(group string, mes schema.Message, m mentions) error {
	log.Printf("ThreadId:%d -> show mention group:%v, %v", mes.ThreadId, group, m)
	body := fmt.Sprintf("%s は存在しません", group)
	list, ok := conf.MentionLists[group]
	if ok {
		body = fmt.Sprintf("%s のメンバーは\n %v です", group, userNameJoin(list.Users))
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: mes.ThreadId}
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

func cc(group string, mes schema.Message, m mentions) error {
	log.Printf("ThreadId:%d -> cc mention group:%v, %v", mes.ThreadId, group, m)
	var ccIDs, directToUserIDs string
	body := fmt.Sprintf("%s は存在しません", group)
	list, ok := conf.MentionLists[group]
	if ok {
		body = fmt.Sprintf("%s にccします", group)
		ccIDs = userJoin(list.Users)
		if mes.DirectMessage {
			directToUserIDs = idJoin(list.Users)
		}
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: mes.ThreadId, CC: ccIDs, DirectToUserIDs: directToUserIDs}
	_, err := client.PostMessage(postMes)
	return err
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
