package fritzbox_upnp

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// curl http://fritz.box:49000/igddesc.xml
// curl http://fritz.box:49000/any.xml
// curl http://fritz.box:49000/igdconnSCPD.xml
// curl http://fritz.box:49000/igdicfgSCPD.xml
// curl http://fritz.box:49000/igddslSCPD.xml
// curl http://fritz.box:49000/igd2ipv6fwcSCPD.xml

const text_xml = `text/xml; charset="utf-8"`

var ErrResultWithoutChardata = errors.New("result without chardata")

type Root struct {
	BaseUrl  string
	Device   UpnpDevice `xml:"device"`
	Services map[string]*UpnpService
}

type UpnpDevice struct {
	root *Root

	DeviceType       string `xml:"deviceType"`
	FriendlyName     string `xml:"friendlyName"`
	Manufacturer     string `xml:"manufacturer"`
	ManufacturerUrl  string `xml:"manufacturerURL"`
	ModelDescription string `xml:"modelDescription"`
	ModelName        string `xml:"modelName"`
	ModelNumber      string `xml:"modelNumber"`
	ModelUrl         string `xml:"modelURL"`
	UDN              string `xml:"UDN"`

	Services []*UpnpService `xml:"serviceList>service"`
	Devices  []*UpnpDevice  `xml:"deviceList>device"`

	PresentationUrl string `xml:"presentationURL"`
}

type UpnpService struct {
	Device *UpnpDevice

	ServiceType string `xml:"serviceType"`
	ServiceId   string `xml:"serviceId"`
	ControlUrl  string `xml:"controlURL"`
	EventSubUrl string `xml:"eventSubURL"`
	SCPDUrl     string `xml:"SCPDURL"`

	Actions        map[string]*UpnpAction
	StateVariables []*UpnpStateVariable
}

type upnpScpd struct {
	Actions        []*UpnpAction        `xml:"actionList>action"`
	StateVariables []*UpnpStateVariable `xml:"serviceStateTable>stateVariable"`
}

type UpnpAction struct {
	service *UpnpService

	Name        string      `xml:"name"`
	Arguments   []*Argument `xml:"argumentList>argument"`
	ArgumentMap map[string]*Argument
}

func (a *UpnpAction) IsGetOnly() bool {
	for _, a := range a.Arguments {
		if a.Direction == "in" {
			return false
		}
	}
	return len(a.Arguments) > 0
}

type Argument struct {
	Name                 string `xml:"name"`
	Direction            string `xml:"direction"`
	RelatedStateVariable string `xml:"relatedStateVariable"`
	StateVariable        *UpnpStateVariable
}

type UpnpStateVariable struct {
	Name         string `xml:"name"`
	DataType     string `xml:"dataType"`
	DefaultValue string `xml:"defaultValue"`
}

type Result map[string]interface{}

func (r *Root) load() error {
	igddesc, err := http.Get(
		fmt.Sprintf("%s/igddesc.xml", r.BaseUrl),
	)

	if err != nil {
		return err
	}

	dec := xml.NewDecoder(igddesc.Body)

	err = dec.Decode(r)
	if err != nil {
		return err
	}

	r.Services = make(map[string]*UpnpService)
	return r.Device.fillServices(r)
}

func (d *UpnpDevice) fillServices(r *Root) error {
	d.root = r

	for _, s := range d.Services {
		s.Device = d

		response, err := http.Get(r.BaseUrl + s.SCPDUrl)
		if err != nil {
			return err
		}

		var scpd upnpScpd

		dec := xml.NewDecoder(response.Body)
		err = dec.Decode(&scpd)
		if err != nil {
			return err
		}

		s.Actions = make(map[string]*UpnpAction)
        for _, a := range scpd.Actions {
            s.Actions[a.Name] = a
        }
		s.StateVariables = scpd.StateVariables

		for _, a := range s.Actions {
			a.service = s
			a.ArgumentMap = make(map[string]*Argument)

			for _, arg := range a.Arguments {
				for _, svar := range s.StateVariables {
					if arg.RelatedStateVariable == svar.Name {
						arg.StateVariable = svar
					}
				}

				a.ArgumentMap[arg.Name] = arg
			}
		}

		r.Services[s.ServiceType] = s
	}
	for _, d2 := range d.Devices {
		err := d2.fillServices(r)
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *UpnpAction) Call() (Result, error) {
	bodystr := fmt.Sprintf(`
        <?xml version='1.0' encoding='utf-8'?> 
        <s:Envelope s:encodingStyle='http://schemas.xmlsoap.org/soap/encoding/' xmlns:s='http://schemas.xmlsoap.org/soap/envelope/'> 
            <s:Body> 
                <u:%s xmlns:u='%s' /> 
            </s:Body>
        </s:Envelope>
    `, a.Name, a.service.ServiceType)

	url := a.service.Device.root.BaseUrl + a.service.ControlUrl
	body := strings.NewReader(bodystr)

	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}

	action := fmt.Sprintf("%s#%s", a.service.ServiceType, a.Name)

	req.Header["Content-Type"] = []string{text_xml}
	req.Header["SoapAction"] = []string{action}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	data := new(bytes.Buffer)
	data.ReadFrom(resp.Body)

	// fmt.Printf(data.String())
	return a.parseSoapResponse(data)

}

func (a *UpnpAction) parseSoapResponse(r io.Reader) (Result, error) {
	res := make(Result)
	dec := xml.NewDecoder(r)

	for {
		t, err := dec.Token()
		if err == io.EOF {
			return res, nil
		}

		if err != nil {
			return nil, err
		}

		if se, ok := t.(xml.StartElement); ok {
			arg, ok := a.ArgumentMap[se.Name.Local]

			if ok {
				t2, err := dec.Token()
				if err != nil {
					return nil, err
				}

				var val string
				switch element := t2.(type) {
				case xml.EndElement:
					val = ""
				case xml.CharData:
					val = string(element)
				default:
					return nil, ErrResultWithoutChardata
				}

				converted, err := convertResult(val, arg)
				if err != nil {
					return nil, err
				}
				res[arg.StateVariable.Name] = converted
			}
		}

	}
}

func convertResult(val string, arg *Argument) (interface{}, error) {
	switch arg.StateVariable.DataType {
	case "string":
		return val, nil
	case "boolean":
		return bool(val == "1"), nil

	case "ui1", "ui2", "ui4":
		res, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return nil, err
		}
		return uint32(res), nil
	default:
		return nil, fmt.Errorf("unknown datatype: %s", arg.StateVariable.DataType)

	}
}

func LoadServices(device string, port uint16) (*Root, error) {
	var root = &Root{
		BaseUrl: fmt.Sprintf("http://%s:%d", device, port),
	}

	err := root.load()
	if err != nil {
		return nil, err
	}

	return root, nil
}