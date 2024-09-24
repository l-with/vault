// Code generated by "enumer -type=clientType -trimprefix=clientType -transform=snake"; DO NOT EDIT.

package vault

import (
	"fmt"
)

const _clientTypeName = "confidentialpublic"

var _clientTypeIndex = [...]uint8{0, 12, 18}

func (i clientType) String() string {
	if i < 0 || i >= clientType(len(_clientTypeIndex)-1) {
		return fmt.Sprintf("clientType(%d)", i)
	}
	return _clientTypeName[_clientTypeIndex[i]:_clientTypeIndex[i+1]]
}

var _clientTypeValues = []clientType{0, 1}

var _clientTypeNameToValueMap = map[string]clientType{
	_clientTypeName[0:12]:  0,
	_clientTypeName[12:18]: 1,
}

// clientTypeString retrieves an enum value from the enum constants string name.
// Throws an error if the param is not part of the enum.
func clientTypeString(s string) (clientType, error) {
	if val, ok := _clientTypeNameToValueMap[s]; ok {
		return val, nil
	}
	return 0, fmt.Errorf("%s does not belong to clientType values", s)
}

// clientTypeValues returns all values of the enum
func clientTypeValues() []clientType {
	return _clientTypeValues
}

// IsAclientType returns "true" if the value is listed in the enum definition. "false" otherwise
func (i clientType) IsAclientType() bool {
	for _, v := range _clientTypeValues {
		if i == v {
			return true
		}
	}
	return false
}
