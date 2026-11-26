// Команда записывает маленький вымышленный справочник для стенда.
package main

import (
	"log"
	"os"

	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/snapshot"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("укажите путь для снимка стендового справочника")
	}
	data := snapshot.Data{
		Streets: []index.Street{{Name: "Пилотная", Type: "улица", Region: "77"}},
		Houses:  map[uint32][]index.HouseRef{0: index.HouseRefsOf("1")},
	}
	if _, err := snapshot.Write(os.Args[1], data, 20261126); err != nil {
		log.Fatal(err)
	}
}
