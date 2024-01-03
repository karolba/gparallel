package terminalscreen

import "golang.org/x/exp/slices"

// SelectGraphicRenditionAttribute is a list representing a single SGR attribute
// - in the format the danielgatis/go-vte gives us.
type SelectGraphicRenditionAttribute [][]uint16

type SGRList []SelectGraphicRenditionAttribute

func (s SelectGraphicRenditionAttribute) addToSGRAttributeList(sgrAttributeList *SGRList) {
	// don't add the same SGR twice by removing the previous one from the slice
	// TODO: make this more elegant
	if len(s) > 0 && len(s[0]) > 0 {
		for i, sgr := range *sgrAttributeList {
			if len(sgr) > 0 && len(sgr[0]) > 0 && sgr[0][0] == s[0][0] {
				*sgrAttributeList = append((*sgrAttributeList)[:i], (*sgrAttributeList)[i+1:]...)
			}
		}
	}
	*sgrAttributeList = append(*sgrAttributeList, s)
}

func (s SelectGraphicRenditionAttribute) isUnsetAll() bool {
	// https://terminalguide.namepad.de/seq/csi_sm/
	// "If no attribute is given or attribute = 0, unset all attributes."
	if len(s) == 0 {
		return true
	}
	if len(s) == 1 && len(s[0]) == 1 && s[0][0] == 0 {
		return true
	}
	return false
}

func (s SelectGraphicRenditionAttribute) toCSI() string {
	return "\033[" + paramsToString(s) + "m"
}

// TODO: the two following methods, is there a better way to do them in go?
func (s SelectGraphicRenditionAttribute) equals(other SelectGraphicRenditionAttribute) bool {
	if len(s) != len(other) {
		return false
	}
	for i, param := range s {
		if !slices.Equal(param, other[i]) {
			return false
		}
	}
	return true
}

func (s SGRList) equals(other SGRList) bool {
	if len(s) != len(other) {
		return false
	}
	for i, sgr := range s {
		if !sgr.equals(other[i]) {
			return false
		}
	}
	return true
}
