package responder

import (
	"bufio"
	"bytes"
	"crypto"
	_ "crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"golang.org/x/crypto/ocsp"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type OCSPResponder struct {
	IndexFile    string
	RespKeyFile  string //do NOT keep this in memory
	RespCertFile string
	CaCertFile   string
	Strict       bool
	Port         int
	Address      string
	Ssl          bool
	CaCert       *x509.Certificate
	RespCert     *x509.Certificate
}

func Responder() *OCSPResponder {
	return &OCSPResponder{
		IndexFile:    "index.txt",
		RespKeyFile:  "responder.key",
		RespCertFile: "responder.crt",
		CaCertFile:   "ca.crt",
		Strict:       false,
		Port:         8888,
		Address:      "",
		Ssl:          false,
		CaCert:       nil,
		RespCert:     nil,
	}
}

func (self *OCSPResponder) makeHandler() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Print("Got request")
		if self.Strict && r.Header.Get("Content-Type") != "application/ocsp-request" {
			fmt.Println("Strict mode requires correct Content-Type header")
			return
		}

		b := new(bytes.Buffer)
		switch r.Method {
		case "POST":
			b.ReadFrom(r.Body)
		case "GET":
			gd, _ := base64.StdEncoding.DecodeString(r.URL.Path[1:])
			b.Read(gd)
		default:
			fmt.Println("Unsupported request method")
			return
		}

		w.Header().Set("Content-Type", "application/ocsp-response")
		resp, err := self.verify(b.Bytes())
		if err != nil {
			log.Print(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		log.Print("Writing response...")
		w.Write(resp)
	}
}

//I only know of two types, but more can be added later
const (
	StatusValid   = 'V'
	StatusRevoked = 'R'
)

type IndexEntry struct {
	Status byte
	Serial uint64 //this probably should be a big.Int but I don't see how it would get bigger than a 64 byte int
	//todo add revoke time and maybe reason
	DistinguishedName string
}

//function to parse the index file, return as a list of IndexEntries
func (self *OCSPResponder) parseIndex() ([]IndexEntry, error) {
	var ret []IndexEntry
	if file, err := os.Open(self.IndexFile); err == nil {
		defer file.Close()
		s := bufio.NewScanner(file)
		for s.Scan() {
			var ie IndexEntry
			ln := strings.Fields(s.Text())
			//probably check for error
			ie.Status = []byte(ln[0])[0]
			//handle strconv errors later
			if ie.Status == StatusValid {
				ie.Serial, _ = strconv.ParseUint(ln[2], 16, 64)
				ie.DistinguishedName = ln[4]
			} else if ie.Status == StatusRevoked {
				ie.Serial, _ = strconv.ParseUint(ln[3], 16, 64)
				ie.DistinguishedName = ln[5]
			} else {
				//invalid status or bad line. just carry on
				continue
			}
			ret = append(ret, ie)
		}
	} else {
		return nil, errors.New("Could not open index file")
	}
	return ret, nil
}

func (self *OCSPResponder) getIndexEntry(s uint64) (*IndexEntry, error) {
	ents, err := self.parseIndex()
	if err != nil {
		return nil, err
	}
	for _, ent := range ents {
		if ent.Serial == s {
			return &ent, nil
		}
	}
	return nil, errors.New("Serial not found")
}

//function to get and hash the CA cert public key
func parseCertFile(filename string) (*x509.Certificate, error) {
	ct, err := ioutil.ReadFile(filename)
	if err != nil {
		//print out error message here
		return nil, err
	}
	block, _ := pem.Decode(ct)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return cert, nil
}

func parseKeyFile(filename string) (interface{}, error) {
	kt, err := ioutil.ReadFile(filename)
	if err != nil {
		//print out error message here
		return nil, err
	}
	block, _ := pem.Decode(kt)
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

//takes the der encoded ocsp request and verifies it
func (self *OCSPResponder) verify(rawreq []byte) ([]byte, error) {
	req, err := ocsp.ParseRequest(rawreq)
	if err != nil {
		return nil, err
	}
	ent, err := self.getIndexEntry(req.SerialNumber.Uint64())
	if err != nil {
		//cert not found status response is ocsp.Unknown
		return nil, err
	}
	log.Print(ent)

	var status int
	if ent.Status == StatusRevoked {
		status = ocsp.Revoked
		fmt.Println("This certificate is revoked!")
	} else {
		status = ocsp.Good
	}

	keyi, err := parseKeyFile(self.RespKeyFile)
	if err != nil {
		return nil, err
	}

	key, ok := keyi.(crypto.Signer)
	if !ok {
		return nil, errors.New("Could not make key a signer...")
	}

	now := time.Now().Truncate(time.Minute)
	rtemplate := ocsp.Response{
		Status:           status,
		SerialNumber:     req.SerialNumber,
		Certificate:      self.RespCert,
		RevocationReason: ocsp.Unspecified,
		RevokedAt:        now.AddDate(0, -1, 0), //get real date later...
		ThisUpdate:       now.AddDate(0, -2, 0),
		NextUpdate:       now.AddDate(0, 30, 0),
		ExtraExtensions:  nil,
	}

	resp, err := ocsp.CreateResponse(self.CaCert, self.RespCert, rtemplate, key)
	if err != nil {
		log.Print(err)
		return nil, err
	}

	return resp, err
}

func (self *OCSPResponder) Serve() error {
	//the certs should not change, so lets keep it in memory
	cacert, err := parseCertFile(self.CaCertFile)
	if err != nil {
		return err
	}
	respcert, err := parseCertFile(self.RespCertFile)
	if err != nil {
		return err
	}
	self.CaCert = cacert
	self.RespCert = respcert

	handler := self.makeHandler()
	http.HandleFunc("/", handler)
	http.ListenAndServe(fmt.Sprintf("%s:%d", self.Address, self.Port), nil)
	return nil
}
