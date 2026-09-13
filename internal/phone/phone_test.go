package phone

import "testing"

func TestNormalizePhoneCollapsesOneCustomer(t *testing.T) {
	// Every spelling a station might write for the same customer.
	same := []string{
		"+92-300-1234567",
		"+92 300 1234567",
		"+923001234567",
		"0300-1234567",
		"0300 1234567",
		"03001234567",
		"0092 300 1234567",
		"92 300 1234567",
		"(0300) 123 4567",
	}
	want := "+923001234567"
	for _, in := range same {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizePhoneLeavesNonNumbersAlone(t *testing.T) {
	for _, in := range []string{"", "LOYALTY-88", "walk-in", "ACC 4412", "123"} {
		if got := Normalize(in); got != in {
			t.Errorf("Normalize(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestNormalizePhoneOtherCountry(t *testing.T) {
	if got := NormalizeCC("020 7946 0958", "44"); got != "+442079460958" {
		t.Errorf("got %q, want +442079460958", got)
	}
	// No country code configured: a national number keeps its own digits
	// rather than being given someone else's dialling code.
	if got := NormalizeCC("03001234567", ""); got != "+03001234567" {
		t.Errorf("got %q, want +03001234567", got)
	}
}

func TestNormalizePhoneIsIdempotent(t *testing.T) {
	once := Normalize("0300-1234567")
	if twice := Normalize(once); twice != once {
		t.Errorf("not idempotent: %q -> %q", once, twice)
	}
}
