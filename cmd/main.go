package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrg/frontmatter"
	"github.com/gofrs/uuid"
	"github.com/nats-io/nats.go"
)

// =====================================================================
// Configuration
// =====================================================================

var (
	natsURL    = getEnv("NATS_URL", "nats://localhost:4222")
	configURL  = getEnv("CONFIG_URL", "http://localhost:8080/alerts")
	mailAPIURL = getEnv("MAIL_API_URL", "https://mail-api.edu.forestier.re/mail")
)

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// =====================================================================
// Structures
// =====================================================================

type Alert struct {
	Id         *uuid.UUID `json:"id"`
	Email      string     `json:"email"`
	All        bool       `json:"all"`
	ResourceId *uuid.UUID `json:"resource_id"`
}

type Event struct {
	Id           string `json:"UID"`
	Dtstamp      string `json:"DTSTAMP"`
	Dtstart      string `json:"DTSTART"`
	Dtend        string `json:"DTEND"`
	Description  string `json:"DESCRIPTION"`
	Location     string `json:"LOCATION"`
	Created      string `json:"CREATED"`
	LastModified string `json:"LAST-MODIFIED"`
	ResourceID   string `json:"RESOURCE-ID"`
}

type MailMatter struct {
	Subject string `yaml:"subject"`
}

// =====================================================================
// Templates embarqués
// =====================================================================

//go:embed templates
var embeddedTemplates embed.FS

func formatICalDate(input string) string {
	t, err := time.Parse("20060102T150405Z", input)
	if err != nil {
		return input // fallback brut
	}
	return t.Format("02/01/2006 15:04")
}

func GetStringFromEmbeddedTemplate(templatePath string, body interface{}) (string, MailMatter, error) {
	var matter MailMatter

	temp, err := template.New(filepath.Base(templatePath)).
		Funcs(template.FuncMap{
			"formatDate": formatICalDate,
			"cleanDescription": func(desc string) string {
				return strings.ReplaceAll(desc, `\n`, "\n")
			},
		}).
		ParseFS(embeddedTemplates, templatePath)
	if err != nil {
		return "", matter, err
	}

	var tpl bytes.Buffer
	if err = temp.Execute(&tpl, body); err != nil {
		return "", matter, err
	}

	mailContent, err := frontmatter.Parse(strings.NewReader(tpl.String()), &matter)
	if err != nil {
		return "", matter, err
	}

	return string(mailContent), matter, nil
}

// =====================================================================
// Envoi d'email via l'API
// =====================================================================

func sendEmail(to, subject, content string) error {
	payload := map[string]interface{}{
		"recipient": to,
		"subject":   subject,
		"content":   content,
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("erreur de sérialisation JSON: %v", err)
	}

	req, err := http.NewRequest("POST", mailAPIURL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return fmt.Errorf("erreur de création de la requête HTTP: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	token := os.Getenv("MAIL_TOKEN")
	if token == "" {
		return fmt.Errorf("MAIL_TOKEN non défini")
	}
	req.Header.Set("Authorization", token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("erreur lors de l'exécution de la requête: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("échec de l'envoi du mail, status: %d, body: %s", resp.StatusCode, string(body))
	}
	return nil
}

// =====================================================================
// Récupération des alertes
// =====================================================================

func fetchAlerts() ([]Alert, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(configURL)
	if err != nil {
		return nil, fmt.Errorf("erreur de récupération des alertes: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code %d de l'API Config", resp.StatusCode)
	}

	var alerts []Alert
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		return nil, fmt.Errorf("erreur de décodage des alertes: %v", err)
	}
	return alerts, nil
}

// =====================================================================
// Traitement des messages NATS
// =====================================================================

func processMessage(m *nats.Msg) {
	var event Event
	if err := json.Unmarshal(m.Data, &event); err != nil {
		log.Printf("Erreur de décodage de l'événement: %v", err)
		return
	}

	isNew := event.LastModified == ""
	log.Printf("Événement reçu (ID: %s, Nouveau: %t)", event.Id, isNew)

	alerts, err := fetchAlerts()
	if err != nil {
		log.Printf("Erreur lors de la récupération des alertes: %v", err)
		return
	}

	for _, alert := range alerts {
		apply := alert.All ||
			(alert.ResourceId != nil &&
				strings.EqualFold(event.ResourceID, alert.ResourceId.String()))

		if apply {
			const templatePath = "templates/alert.txt"
			body := map[string]interface{}{
				"Event": event,
				"IsNew": isNew,
			}

			htmlContent, matter, err := GetStringFromEmbeddedTemplate(templatePath, body)
			if err != nil {
				log.Printf("Erreur template pour l'alerte %s: %v", alert.Id, err)
				continue
			}

			if err = sendEmail(alert.Email, matter.Subject, htmlContent); err != nil {
				log.Printf("Échec envoi à %s (alerte %s): %v",
					maskEmail(alert.Email),
					alert.Id,
					err)
				continue
			}
			log.Printf("Email envoyé à %s", maskEmail(alert.Email))
		}
	}
}

func maskEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return "***@***"
	}
	return fmt.Sprintf("%s***@***%s",
		string(parts[0][0]),
		string(parts[1][len(parts[1])-1]))
}

// =====================================================================
// Main
// =====================================================================

func main() {
	nc, err := nats.Connect(natsURL,
		nats.MaxReconnects(5),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			log.Printf("Déconnecté de NATS: %v", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Printf("Reconnecté à NATS @ %s", nc.ConnectedUrl())
		}))
	if err != nil {
		log.Fatal("Erreur de connexion à NATS:", err)
	}
	defer nc.Close()

	_, err = nc.Subscribe("TIMETABLE.ALERTER", func(m *nats.Msg) {
		processMessage(m)
	})
	if err != nil {
		log.Fatal("Erreur lors de la souscription:", err)
	}

	log.Printf("Service démarré - En écoute sur %s", natsURL)
	select {} // Bloque indéfiniment
}
